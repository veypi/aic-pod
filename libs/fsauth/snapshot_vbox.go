package fsauth

import (
	"github.com/veypi/vbox"
	"os"
	"path/filepath"
)

func (p *Policy) Snapshot(sid string) vbox.FSRuleSet { return p.snapshotWith(sid, nil) }

// Native metadata protection is an ordinary rule group below explicit grants.
func (p *Policy) SnapshotForNative(sid, workdir, cmd string) vbox.FSRuleSet {
	var protected []vbox.Rule
	if workdir != "" && filepath.Base(cmd) != "git" && filepath.Base(cmd) != "git.exe" {
		for _, name := range []string{".git", ".aws"} {
			target := filepath.Join(workdir, name)
			if st, err := os.Stat(target); err == nil && st.IsDir() {
				for _, pattern := range vbox.DualForms(target) {
					protected = append(protected, vbox.Rule{Pattern: pattern, Effect: vbox.EffRO, Class: vbox.ClassBuiltin})
				}
			}
		}
	}
	return p.snapshotWith(sid, protected)
}
func (p *Policy) snapshotWith(sid string, protected []vbox.Rule) vbox.FSRuleSet {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var rules []vbox.Rule
	for _, target := range p.grants[sid] {
		for _, pattern := range vbox.DualForms(target) {
			rules = append(rules, vbox.Rule{Pattern: pattern, Effect: vbox.EffRW, Class: vbox.ClassTemp})
		}
	}
	rules = append(rules, p.rules...)
	rules = append(rules, protected...)
	roots := append(append([]string(nil), p.baseRoots...), p.decideCaches...)
	if sid != "" && p.sessionDir != "" {
		roots = append(roots, p.sessionDir+"/"+sid)
	}
	for _, root := range vbox.DedupClean(roots) {
		rules = append(rules, vbox.Rule{Pattern: root, Effect: vbox.EffRW, Class: vbox.ClassConvenience})
	}
	fallback := vbox.EffDeny
	if p.openMode {
		fallback = vbox.EffRW
	}
	return vbox.FSRuleSet{Rules: rules, DefaultWrite: fallback}
}
