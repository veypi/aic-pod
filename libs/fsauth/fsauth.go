// Package fsauth owns host filesystem authorization state. Parsing and matching
// are implemented by vbox; snapshots retain configuration order (first match).
package fsauth

import (
	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/vbox"
	"os"
	"sync"
)

type Policy struct {
	mu                            sync.RWMutex
	workDir, sessionDir, stateDir string
	openMode                      bool
	rules                         []vbox.Rule
	grants                        map[string][]string
	baseRoots, decideCaches       []string
}

func New() (*Policy, error) {
	p := &Policy{grants: map[string][]string{}}
	dir, err := cfg.StateDir()
	if err != nil {
		return p, err
	}
	p.stateDir = vbox.Canonical(dir)
	p.sessionDir = p.stateDir + "/sessions"
	return p, p.Reconcile()
}

// Reconcile compiles before publishing. A failed reload leaves the last valid
// table intact, including its default policy.
func (p *Policy) Reconcile() error {
	a := cfg.AuthSnapshot()
	rules, err := vbox.CompileFSRules(a.FsRules, vbox.ClassCfg)
	if err != nil {
		return err
	}
	defaults := defaultDenyPaths()
	for i := range defaults {
		defaults[i] = "deny:" + defaults[i]
	}
	builtin, err := vbox.CompileFSRules(defaults, vbox.ClassBuiltin)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.openMode = a.FsPolicy == cfg.PolicyOpen
	p.rules = append(rules, builtin...)
	p.rebuildBaseRootsLocked()
	return nil
}

func (p *Policy) rebuildBaseRootsLocked() {
	roots := append([]string{p.workDir, os.TempDir()}, tempRoots()...)
	p.baseRoots = vbox.DedupClean(vbox.DualList(roots))
	p.decideCaches = vbox.DedupClean(vbox.DualList(cacheRootDirs()))
}
func (p *Policy) SetWorkDir(wd string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.workDir = ""
	if wd != "" {
		p.workDir = vbox.Canonical(wd)
	}
	p.rebuildBaseRootsLocked()
}
func (p *Policy) Grant(sid, target string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	target = vbox.Canonical(target)
	p.grants[sid] = vbox.DedupClean(append([]string{target}, p.grants[sid]...))
}
func (p *Policy) OpenMode() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.openMode
}
func (p *Policy) SessionGrants(sid string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]string(nil), p.grants[sid]...)
}
func (p *Policy) DropSession(sid string) { p.mu.Lock(); defer p.mu.Unlock(); delete(p.grants, sid) }

func appendEnvDirs(dirs []string, names ...string) []string {
	for _, name := range names {
		if v := os.Getenv(name); v != "" {
			dirs = append(dirs, v)
		}
	}
	return dirs
}
