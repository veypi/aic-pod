package netauth

import (
	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/vbox"
)

func (p *Policy) Snapshot(sid string) vbox.NetRuleSet {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var rows []vbox.NetRule
	for _, g := range p.grants[sid] {
		rows = append(rows, vbox.NetRule{HostPort: g.String(), Allow: true})
	}
	rows = append(rows, p.rules...)
	rows = append(rows, p.builtin...)
	return vbox.NetRuleSet{Rules: rows, Default: p.mode == cfg.PolicyOpen}
}
