// Package netauth enforces exact host/port rules for net and ssh. Explicit
// denies always win; allows and per-session grants may open unmatched targets.
// Rules come from executor-local configuration, never from runtime approval.
package netauth

import (
	"net/netip"
	"strconv"
	"strings"
	"sync"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/policy"
)

type Entry = policy.Entry

var ParseEntry = policy.ParseEntry
var ValidateEntries = policy.ValidateEntries

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

// Allowed 判定目标连通性：命中 deny → 拒，除非存在更具体的 allow（具体度优先：
// 端口数字 > *；同精度 deny 胜）。policy=open 时未命中 deny 一律放；policy=deny
// 时仅 allow（内建 + cfg + sid 临时 grant）放行。临时 grant 不压 deny（只有
// 内建/cfg allow 参与具体度比较）。host 归一化与条目同口径。
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
	cfgHit := false
	for _, list := range [][]Entry{p.builtin, p.allow} {
		for _, e := range list {
			if entryMatch(e, q) {
				cfgHit = true
			}
		}
	}

	if p.mode == cfg.PolicyOpen {
		return true
	}
	if cfgHit {
		return true
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
// deny 剔除被同 host 具体端口 allow 压过的端口 * 条目（具体度优先；同精度 deny
// 胜）——deny 模式下未放行端口由基线全拒兜底，剔除不降低隔离；grant 不参与
// 剔除（不压 deny，与 Allowed 同口径）。
func (p *Policy) Snapshot(sid string) (deny, allow []Entry) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	allow = append(allow, p.builtin...)
	allow = append(allow, p.allow...)
	allow = append(allow, p.grants[sid]...)
	deny = append(deny, p.deny...)
	return deny, allow
}

// portNarrowedByAllow 报告端口 * 的 deny 条目是否被同 host 的具体端口 allow
// 压过（具体度优先）。
func portNarrowedByAllow(d Entry, allow []Entry) bool {
	if d.Port != "*" {
		return false
	}
	for _, a := range allow {
		if a.Host == d.Host && a.Port != "*" {
			return true
		}
	}
	return false
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

func (p *Policy) DropSession(sid string) { p.mu.Lock(); defer p.mu.Unlock(); delete(p.grants, sid) }

func (p *Policy) ResetTemporary() { p.mu.Lock(); defer p.mu.Unlock(); p.grants = map[string][]Entry{} }
