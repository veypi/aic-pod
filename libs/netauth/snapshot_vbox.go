package netauth

// snapshot_vbox.go 是 netauth 状态层 → vbox.NetRuleSet 的桥（与 fsauth
// snapshot_vbox.go 同一定位：状态层留 pod，vbox 表在此发射，供 vsh 引擎
// host 端 NetClient 消费）。语义映射（old last-wins → new first-wins）：
//   - temp grant 由「表外兜底」提为表头（v4：temp 行插表头压一切）；
//   - cfg 行组内反转；builtin allow 行在其后（old 中 cfg 可覆盖 builtin，
//     new 首命中保持同序关系）；
//   - Default = net_policy（open 放行 / deny 拒绝）。

import (
	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/vbox"
)

// SnapshotVbox 返回 sid 的 vbox 网络规则表（首命中生效）。
func (p *Policy) SnapshotVbox(sid string) vbox.NetRuleSet {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var rules []vbox.NetRule
	// temp（表头）：sid 临时 grant。
	for _, g := range p.grants[sid] {
		rules = append(rules, vbox.NetRule{HostPort: g.String(), Allow: true})
	}
	// cfg（组内反转）→ builtin allow（组内反转）。
	for i := len(p.rules) - 1; i >= p.builtin; i-- {
		r := p.rules[i]
		rules = append(rules, vbox.NetRule{HostPort: r.e.String(), Allow: r.allow})
	}
	for i := p.builtin - 1; i >= 0; i-- {
		r := p.rules[i]
		rules = append(rules, vbox.NetRule{HostPort: r.e.String(), Allow: r.allow})
	}
	return vbox.NetRuleSet{Rules: rules, Default: p.mode == cfg.PolicyOpen}
}

