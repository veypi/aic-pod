// Package netauth owns net/ssh authorization state, using vbox rule evaluation.
package netauth

import (
	"fmt"
	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/vbox"
	"net"
	"strconv"
	"sync"
)

type Entry = vbox.Entry

var ParseEntry = vbox.ParseEntry

type Selector func(cfg.AuthCfg) (string, []string)

func NetKeys(a cfg.AuthCfg) (string, []string) { return a.NetPolicy, a.NetRules }
func SshKeys(a cfg.AuthCfg) (string, []string) { return a.SshPolicy, a.SshRules }

type Policy struct {
	mu             sync.RWMutex
	sel            Selector
	mode           string
	rules, builtin []vbox.NetRule
	grants         map[string][]Entry
}

func New(sel Selector, builtinAllow ...string) (*Policy, error) {
	p := &Policy{sel: sel, grants: map[string][]Entry{}}
	for _, raw := range builtinAllow {
		e, err := vbox.ParseEntry(raw)
		if err != nil {
			return p, err
		}
		p.builtin = append(p.builtin, vbox.NetRule{HostPort: e.String(), Allow: true})
	}
	return p, p.Reconcile()
}
func (p *Policy) Reconcile() error {
	mode, rows := p.sel(cfg.AuthSnapshot())
	return p.Configure(mode, rows)
}
func (p *Policy) Configure(mode string, rows []string) error {
	if mode != cfg.PolicyOpen && mode != cfg.PolicyDeny {
		return fmt.Errorf("invalid network policy %q", mode)
	}
	var compiled []vbox.NetRule
	for _, raw := range rows {
		allow, e, err := vbox.ParseTargetRule(raw)
		if err != nil {
			return err
		}
		compiled = append(compiled, vbox.NetRule{HostPort: e.String(), Allow: allow})
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mode, p.rules = mode, compiled
	return nil
}
func (p *Policy) Mode() string   { p.mu.RLock(); defer p.mu.RUnlock(); return p.mode }
func (p *Policy) OpenMode() bool { return p.Mode() == cfg.PolicyOpen }
func (p *Policy) Grant(sid string, e Entry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	next := []Entry{e}
	for _, g := range p.grants[sid] {
		if g != e {
			next = append(next, g)
		}
	}
	p.grants[sid] = next
}
func (p *Policy) Allowed(sid, host string, port int) bool {
	return p.Snapshot(sid).Match(net.JoinHostPort(host, strconv.Itoa(port)))
}
func (p *Policy) DropSession(sid string) { p.mu.Lock(); defer p.mu.Unlock(); delete(p.grants, sid) }
