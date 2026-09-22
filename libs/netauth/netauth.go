// Package netauth enforces exact host/port rules for net and ssh as one ordered
// rule table per domain (aic/docs/permission_rules.md): builtin rows first,
// then cfg <domain>_rules (permanent grants append there); the last matching
// row wins and unmatched targets fall back to <domain>_policy. Session-layer
// temp grants apply only when the table does not resolve to deny. Rules come
// from executor-local configuration, never from runtime approval.
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

// ruleEntry 是一条编译后的规则行（allow=true 放行 / false 拒绝）。
type ruleEntry struct {
	allow bool
	e     Entry
}

// Selector 从授权快照选取本实例的（policy, rules）。
type Selector func(cfg.AuthCfg) (string, []string)

// NetKeys 选取 net 域两键。
func NetKeys(a cfg.AuthCfg) (string, []string) { return a.NetPolicy, a.NetRules }

// SshKeys 选取 ssh 域两键。
func SshKeys(a cfg.AuthCfg) (string, []string) { return a.SshPolicy, a.SshRules }

// Policy 是单域目标策略实例（net/ssh 各一）。
type Policy struct {
	mu      sync.RWMutex
	sel     Selector
	mode    string
	rules   []ruleEntry        // 有序规则表：builtin + cfg rules（permanent grant 直接追加在表尾）
	builtin int                // rules 中内建行数量（Reconcile 重编译时保留前缀）
	grants  map[string][]Entry // sid → 临时授权（--temp，不入表）
}

// New 创建实例并从 cfg 加载当前配置。builtinAllow 为内建默认 allow
// 条目（静态表，解析失败静默跳过），恒在表最前（可被 cfg 行覆盖）。
func New(sel Selector, builtinAllow ...string) *Policy {
	p := &Policy{sel: sel, grants: map[string][]Entry{}}
	for _, s := range builtinAllow {
		if e, err := ParseEntry(s); err == nil {
			p.rules = append(p.rules, ruleEntry{allow: true, e: e})
		}
	}
	p.builtin = len(p.rules)
	p.Reconcile()
	return p
}

// Reconcile 从 cfg 授权快照重载（set_config / grant --permanent 变更后调用）。
func (p *Policy) Reconcile() {
	mode, rules := p.sel(cfg.AuthSnapshot())
	p.Configure(mode, rules)
}

// Configure 直接配置（坏条目整条跳过，宁缺毋滥——cfg 校验已在加载/保存路径点名）。
func (p *Policy) Configure(mode string, rules []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mode = cfg.NormalizePolicy(mode, cfg.PolicyDeny)
	compiled := p.rules[:p.builtin]
	for _, raw := range rules {
		if allow, e, err := policy.ParseTargetRule(raw); err == nil {
			compiled = append(compiled, ruleEntry{allow: allow, e: e})
		}
	}
	p.rules = compiled
}

// Mode 返回当前策略（deny/open）。
func (p *Policy) Mode() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.mode
}

// OpenMode 报告当前策略是否为 open（沙箱 profile 生成用）。
func (p *Policy) OpenMode() bool { return p.Mode() == cfg.PolicyOpen }

// Grant 临时授权：条目加入 sid 的 allow 名单（幂等；重启/跨 session 失效，不入表）。
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

// resolve 顺序求值规则表：按拼接序逐条匹配（port 取保守交集语义，任一侧为 *
// 即视为命中），最后命中者胜；未命中 hit=false。
func (p *Policy) resolve(e Entry) (allow, hit bool) {
	for _, r := range p.rules {
		if entryMatch(r.e, e) {
			allow, hit = r.allow, true
		}
	}
	return allow, hit
}

// DenyHit 报告目标的表判定终局是否为 deny（temp grant 校验用：deny 终局
// 拒绝申请——session 层不得放宽表判定的 deny）。目标带 * 端口时判定取
// 保守交集：最后被任一重叠行命中者即终局（申请人可改报更窄条目）。
func (p *Policy) DenyHit(e Entry) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	allow, hit := p.resolve(e)
	return hit && !allow
}

// LastDenyRow 报告目标最后重叠命中的 deny 行序号（1 起，拼接序）与规范形态——
// grant --permanent 的覆盖提示用（与 DenyHit 配合：仅当终局为 deny 时展示）。
func (p *Policy) LastDenyRow(e Entry) (int, string, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	idx := -1
	for i, r := range p.rules {
		if !r.allow && entryMatch(r.e, e) {
			idx = i
		}
	}
	if idx < 0 {
		return 0, "", false
	}
	return idx + 1, p.rules[idx].e.String(), true
}

// Allowed 顺序求值规则表：deny 终局拒，allow 终局放；未命中走 policy 兜底
// （open 放行 / deny 仅内建行、配置行或当前 sid 的临时 grant 已命中者）。
// host 与规则使用同一归一化口径。
func (p *Policy) Allowed(sid, host string, port int) bool {
	q := Entry{Host: host, Port: strconv.Itoa(port)}
	if ip, err := netip.ParseAddr(strings.TrimSuffix(strings.ToLower(host), ".")); err == nil {
		q.Host = ip.String()
	} else {
		q.Host = strings.TrimSuffix(strings.ToLower(host), ".")
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if allow, hit := p.resolve(q); hit {
		return allow
	}
	if p.mode == cfg.PolicyOpen {
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
// 且端口匹配（r 为 * 或相等；q 为 * 时取保守交集——重叠即命中）。
func entryMatch(r, q Entry) bool {
	if r.Host != q.Host {
		return false
	}
	return r.Port == "*" || r.Port == q.Port || q.Port == "*"
}

// Snapshot 返回独立的 deny/allow 快照，供沙箱 profile 生成使用。
// deny 为全部 deny 行（M3 前内核不表达行内洞，fail-closed）；allow 为
// 全部 allow 行（内建与配置行）加 sid 的临时 grant。
func (p *Policy) Snapshot(sid string) (deny, allow []Entry) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, r := range p.rules {
		if r.allow {
			allow = append(allow, r.e)
		} else {
			deny = append(deny, r.e)
		}
	}
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

func (p *Policy) DropSession(sid string) { p.mu.Lock(); defer p.mu.Unlock(); delete(p.grants, sid) }
