// Package netauth 是 host:port 目标策略引擎（三域授权模型的 net/ssh 两域共用，
// 一个包两个实例）：
//
//	判定式（与 fsauth 同形）：deny 命中 → 拒；policy=open → 放；
//	policy=deny → 仅 allow 放行。
//
// 条目形态 host:port：host 小写/去尾点归一、IP 经 netip 归一（归一后字符串
// 相等即匹配）；port 为数字或 *（全端口）；bare host 归一为 host:*。
// user@ 前缀在解析时剥掉（用户是认证细节，不是网络目标）。
//
// net 实例带内建默认 allow localhost:*（loopback 放行——go test/httptest/
// dev server 的命根子；net_deny localhost:* 可反杀，deny 恒优先）。
// ssh 实例无内建条目。
//
// 配置源 = cfg 授权快照（net_policy/net_deny/net_allow、ssh_policy/ssh_deny/
// ssh_allow），Reconcile 重载；坏条目整条跳过（与 fsauth 静默风格一致——
// set_config 侧先用 ValidateEntries 显式校验，运行期不炸）。
// 临时授权 = Grant(sid)（grant <域> --temp），host 进程内存，重启/跨 session
// 失效（与 fsauth grant 同生命周期语义，量小重启清零）。
package netauth

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/veypi/aic-pod/cfg"
)

// Entry 是归一化目标条目（Host 小写/IP 归一，Port 数字串或 *）。
type Entry struct {
	Host string
	Port string
}

// String 返回规范形态 host:port。
func (e Entry) String() string { return e.Host + ":" + e.Port }

// ParseEntry 解析并归一化目标条目：bare host → host:*；剥 user@ 前缀；
// host 小写/去尾点、IP 经 netip 归一；port 须为 1-65535 或 *。
// IPv6 须带括号（[::1]:22）；不带括号的多冒号形态按 bare host 处理（port=*）。
func ParseEntry(s string) (Entry, error) {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:] // 剥 user@（ssh 目标形态 user@host:port）
	}
	if s == "" {
		return Entry{}, fmt.Errorf("empty target")
	}
	if strings.IndexFunc(s, func(r rune) bool { return r == '/' || unicode.IsSpace(r) }) >= 0 {
		return Entry{}, fmt.Errorf("invalid target %q: want host[:port]", s)
	}
	host, port := "", ""
	if strings.HasPrefix(s, "[") {
		h, p, err := net.SplitHostPort(s)
		if err != nil {
			return Entry{}, fmt.Errorf("invalid target %q: %v", s, err)
		}
		host, port = h, p
	} else if strings.Count(s, ":") == 1 {
		h, p, err := net.SplitHostPort(s)
		if err != nil {
			return Entry{}, fmt.Errorf("invalid target %q: %v", s, err)
		}
		host, port = h, p
	} else if strings.Count(s, ":") == 0 {
		host, port = s, "*"
	} else {
		host, port = s, "*" // 不带括号的 bare IPv6
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if ip, err := netip.ParseAddr(host); err == nil {
		host = ip.String()
	}
	if host == "" {
		return Entry{}, fmt.Errorf("invalid target %q: empty host", s)
	}
	if strings.Contains(host, "*") {
		// 通配主机整条拒绝：entryMatch 是归一后字符串相等，* 条目恒惰性
		//（永不匹配）——静默接受会让用户以为生效（set_config 显式校验语义）。
		return Entry{}, fmt.Errorf("invalid target %q: wildcard host is not supported (entries match exact hosts only)", s)
	}
	if port != "*" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return Entry{}, fmt.Errorf("invalid port %q in %q: want 1-65535 or *", port, s)
		}
		port = strconv.Itoa(n)
	}
	return Entry{Host: host, Port: port}, nil
}

// ValidateEntries 批量校验条目合法性（set_config 显式校验用；首个非法条目即报错）。
func ValidateEntries(list []string) error {
	for _, s := range list {
		if _, err := ParseEntry(s); err != nil {
			return err
		}
	}
	return nil
}

// Selector 从授权快照选取本实例的（policy, deny, allow）。
type Selector func(cfg.AuthCfg) (string, []string, []string)

// NetKeys 选取 net 域三键。
func NetKeys(a cfg.AuthCfg) (string, []string, []string) { return a.NetPolicy, a.NetDeny, a.NetAllow }

// SshKeys 选取 ssh 域三键。
func SshKeys(a cfg.AuthCfg) (string, []string, []string) { return a.SshPolicy, a.SshDeny, a.SshAllow }

// Policy 是单域目标策略实例（net/ssh 各一）。
type Policy struct {
	mu      sync.RWMutex
	sel     Selector
	builtin []Entry // 内建默认 allow（net：localhost:*）
	mode    string
	deny    []Entry
	allow   []Entry
	grants  map[string][]Entry // sid → 临时授权（--temp）
}

// New 创建实例并从 cfg 加载当前配置。builtinAllow 为内建默认 allow
// 条目（静态表，解析失败静默跳过）。
func New(sel Selector, builtinAllow ...string) *Policy {
	p := &Policy{sel: sel, grants: map[string][]Entry{}}
	for _, s := range builtinAllow {
		if e, err := ParseEntry(s); err == nil {
			p.builtin = append(p.builtin, e)
		}
	}
	p.Reconcile()
	return p
}

// Reconcile 从 cfg 授权快照重载（set_config / grant --permanent 变更后调用）。
func (p *Policy) Reconcile() {
	mode, deny, allow := p.sel(cfg.AuthSnapshot())
	p.Configure(mode, deny, allow)
}

// Configure 直接配置（坏条目整条跳过，宁缺毋滥）。
func (p *Policy) Configure(mode string, deny, allow []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mode = cfg.NormalizePolicy(mode, cfg.PolicyDeny)
	p.deny = parseAll(deny)
	p.allow = parseAll(allow)
}

func parseAll(list []string) []Entry {
	out := make([]Entry, 0, len(list))
	for _, s := range list {
		if e, err := ParseEntry(s); err == nil {
			out = append(out, e)
		}
	}
	return out
}

// Mode 返回当前策略（deny/open）。
func (p *Policy) Mode() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.mode
}

// OpenMode 报告当前策略是否为 open（沙箱 profile 生成用）。
func (p *Policy) OpenMode() bool { return p.Mode() == cfg.PolicyOpen }

// Grant 临时授权：条目加入 sid 的 allow 名单（幂等；重启/跨 session 失效）。
func (p *Policy) Grant(sid string, e Entry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, g := range p.grants[sid] {
		if g == e {
			return
		}
	}
	p.grants[sid] = append(p.grants[sid], e)
}

// DenyHit 报告条目是否命中 deny 名单（grant 校验用：deny 内拒绝申请）。
// 端口语义取保守交集：任一侧为 * 即视为命中（重叠即拒，申请人可改报更窄条目）。
func (p *Policy) DenyHit(e Entry) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, d := range p.deny {
		if entryMatch(d, e) {
			return true
		}
	}
	return false
}

// Allowed 判定目标连通性：deny 命中 → 拒；open → 放；deny → 仅 allow
// （内建 + cfg + sid 临时 grant）放行。host 归一化与条目同口径。
func (p *Policy) Allowed(sid, host string, port int) bool {
	q := Entry{Host: host, Port: strconv.Itoa(port)}
	if ip, err := netip.ParseAddr(strings.TrimSuffix(strings.ToLower(host), ".")); err == nil {
		q.Host = ip.String()
	} else {
		q.Host = strings.TrimSuffix(strings.ToLower(host), ".")
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, d := range p.deny {
		if entryMatch(d, q) {
			return false
		}
	}
	if p.mode == cfg.PolicyOpen {
		return true
	}
	for _, e := range p.builtin {
		if entryMatch(e, q) {
			return true
		}
	}
	for _, e := range p.allow {
		if entryMatch(e, q) {
			return true
		}
	}
	for _, e := range p.grants[sid] {
		if entryMatch(e, q) {
			return true
		}
	}
	return false
}

// entryMatch 判定规则条目 r 是否覆盖查询条目 q：host 字符串相等（双方已归一）
// 且端口匹配（r 为 * 或相等）。
func entryMatch(r, q Entry) bool {
	if r.Host != q.Host {
		return false
	}
	return r.Port == "*" || r.Port == q.Port || q.Port == "*"
}

// Snapshot 返回 sid 的（deny, allow）条目快照——沙箱 profile 生成用
// （allow = 内建 + cfg + sid 临时 grant；open 模式下沙箱层直接放行不读本快照）。
func (p *Policy) Snapshot(sid string) (deny, allow []Entry) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	deny = append(deny, p.deny...)
	allow = append(allow, p.builtin...)
	allow = append(allow, p.allow...)
	allow = append(allow, p.grants[sid]...)
	return deny, allow
}

// List 返回 sid 视角的当前 allow 清单（规范形态字符串，grant 响应回显用）。
func (p *Policy) List(sid string) []string {
	_, allow := p.Snapshot(sid)
	out := make([]string, 0, len(allow))
	seen := map[string]bool{}
	for _, e := range allow {
		s := e.String()
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
