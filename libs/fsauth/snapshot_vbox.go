package fsauth

// snapshot_vbox.go 是 fsauth 状态层 → vbox 纯 matcher 的桥（design §11.5
// 阶段一：状态层留 pod，vbox 表在此发射）。vsh 引擎 host 端 FS 适配器消费
// 本快照；语义为 v4 first-wins 新行序（temp → cfg/permanent → builtin deny →
// 便利根），旧 decide 路径（last-wins + 便利根表外授写）的语义映射规则：
//   - old: builtin deny 在前、cfg 在后，后命中者胜 → cfg 可覆盖 builtin deny；
//     new: cfg 行排在 builtin deny 之前，首命中即中——覆盖关系保持。
//   - old: 组内后命中者胜 → new: 组内反转（末行提首）。
//   - old: 便利根仅在表判定非 deny 时授写（不入表）→ new: 便利根 rw 行排表尾，
//     builtin deny 在其上——"便利不压 deny"保持。
//   - old: 未命中 effNone → fs_policy 兜底 → new: DefaultWrite = open?RW:Deny。
// grant.go DenyHit 拒批（session 硬底线）已按 2.7.4 于 M3c 删除——新行序下 temp
// 行在表头，天然压一切（含 builtin deny），语义由行序表达。

import (
	"github.com/veypi/vbox"
)

// Snapshot 返回 sid 的 vbox 规则表（纯拼接，每次调用取当次值——grant/cfg
// 动态生效）。首命中生效；DefaultWrite 依 fs_policy。
func (p *Policy) Snapshot(sid string) vbox.FSRuleSet {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var rules []vbox.Rule
	// temp（表头）：sid 临时 grant，canonical 前缀。
	for _, g := range p.grants[sid] {
		rules = append(rules, vbox.Rule{Pattern: g, Effect: vbox.EffRW, Class: vbox.ClassTemp})
	}
	// cfg/permanent（组内反转：old last-wins → new first-wins）。
	for i := len(p.rules) - 1; i >= 0; i-- {
		r := p.rules[i]
		if r.src != "cfg" {
			continue
		}
		rules = appendVboxRule(rules, r)
	}
	// builtin deny（全 deny，组内顺序无关）。
	for _, r := range p.rules {
		if r.src != "builtin" {
			continue
		}
		rules = appendVboxRule(rules, r)
	}
	// 便利根 rw（表尾）：baseRoots + 缓存候选 + 会话区。
	for _, root := range p.convenienceRoots(sid) {
		rules = append(rules, vbox.Rule{Pattern: root, Effect: vbox.EffRW, Class: vbox.ClassConvenience})
	}
	defaultWrite := vbox.EffDeny
	if p.openMode {
		defaultWrite = vbox.EffRW
	}
	// 直接字面构造：pats 已是 canonical 双形态（compileFSRule 产物），
	// 不再过 NewFSRuleSet 的 CanonicalPattern（glob 模式经二次归一会错位）。
	return vbox.FSRuleSet{Rules: rules, DefaultWrite: defaultWrite}
}

// appendVboxRule 展开一条 fsRule 的全部匹配形态为多行 vbox.Rule。
func appendVboxRule(out []vbox.Rule, r fsRule) []vbox.Rule {
	eff := vbox.EffRW
	switch r.eff {
	case effDeny:
		eff = vbox.EffDeny
	case effRO:
		eff = vbox.EffRO
	}
	class := vbox.ClassCfg
	if r.src == "builtin" {
		class = vbox.ClassBuiltin
	}
	for _, pat := range r.pats {
		out = append(out, vbox.Rule{Pattern: pat, Effect: eff, Class: class})
	}
	return out
}

// convenienceRoots 便利根（与 decideRootsLocked 同源：基底 + 缓存 + 会话区，
// 不含 temp grant——temp 已单独占表头）。
func (p *Policy) convenienceRoots(sid string) []string {
	roots := make([]string, 0, len(p.baseRoots)+len(p.decideCaches)+1)
	roots = append(roots, p.baseRoots...)
	roots = append(roots, p.decideCaches...)
	if sid != "" && p.sessionDir != "" {
		roots = append(roots, p.sessionDir+"/"+sid)
	}
	return dedupClean(roots)
}
