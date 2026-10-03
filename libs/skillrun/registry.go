package skillrun

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/veypi/vbox"
	"github.com/veypi/vsh/commands"
)

// Deps is assembled directly by the host; skillrun does not own shell sessions.
type Deps struct {
	SkillsDir string
	RunDir    string
	Manager   *vbox.Manager
	Policy    func(context.Context, string, string) vbox.Policy
	Workdir   func(string) string
	Registry  *commands.Registry
	Fetch     func(context.Context, string, string) ([]byte, *FetchMeta, error)
	Logf      func(string, ...any)
}
type Package struct {
	Name     string
	Dir      string
	Manifest *Manifest
	mu       sync.Mutex
	record   InstallRecord
	disabled bool
	changing bool
	active   int
	service  *serviceInst
}

func (p *Package) Disabled() bool        { p.mu.Lock(); defer p.mu.Unlock(); return p.disabled }
func (p *Package) Record() InstallRecord { p.mu.Lock(); defer p.mu.Unlock(); return p.record }

type Registry struct {
	deps       Deps
	mu         sync.Mutex
	pkgs       map[string]*Package
	closed     bool
	operations map[string]*sync.Mutex
}

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

func New(deps Deps) (*Registry, error) {
	if deps.SkillsDir == "" || deps.RunDir == "" || deps.Manager == nil || deps.Registry == nil {
		return nil, fmt.Errorf("skillrun: SkillsDir, RunDir, Manager and Registry required")
	}
	if err := os.MkdirAll(deps.SkillsDir, 0700); err != nil {
		return nil, err
	}
	return &Registry{deps: deps, pkgs: map[string]*Package{}, operations: map[string]*sync.Mutex{}}, nil
}
func (r *Registry) logf(format string, args ...any) {
	if r.deps.Logf != nil {
		r.deps.Logf(format, args...)
	}
}

// Serialize mutations of a package; calls and other packages remain independent.
func (r *Registry) lockPackage(name string) func() {
	r.mu.Lock()
	m := r.operations[name]
	if m == nil {
		m = &sync.Mutex{}
		r.operations[name] = m
	}
	r.mu.Unlock()
	m.Lock()
	return m.Unlock
}
func (r *Registry) Get(name string) *Package { r.mu.Lock(); defer r.mu.Unlock(); return r.pkgs[name] }
func (r *Registry) List() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.pkgs))
	for name := range r.pkgs {
		out = append(out, name)
	}
	return out
}
func (r *Registry) IsPackageCommand(name string) bool {
	p := r.Get(name)
	return p != nil && p.Manifest != nil
}

func (r *Registry) begin(name string) (*Package, func(), error) {
	r.mu.Lock()
	closed := r.closed
	p := r.pkgs[name]
	r.mu.Unlock()
	if closed {
		return nil, nil, fmt.Errorf("skill registry closed")
	}
	if p == nil {
		return nil, nil, fmt.Errorf("skill package %q not installed", name)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.disabled {
		return nil, nil, fmt.Errorf("skill package %q disabled", name)
	}
	if p.changing {
		return nil, nil, fmt.Errorf("skill package %q busy", name)
	}
	p.active++
	return p, func() { p.mu.Lock(); p.active--; p.mu.Unlock() }, nil
}
func (r *Registry) prepareChange(p *Package) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Manifest != nil && p.Manifest.Kind == KindProcess && p.active > 0 {
		return fmt.Errorf("skill package %q busy: process calls active", p.Name)
	}
	p.changing = true
	return nil
}
func finishChange(p *Package) {
	if p != nil {
		p.mu.Lock()
		p.changing = false
		p.mu.Unlock()
	}
}

func (r *Registry) Uninstall(name string) error {
	unlock := r.lockPackage(name)
	defer unlock()
	p := r.Get(name)
	if p == nil {
		return nil
	}
	if err := r.prepareChange(p); err != nil {
		return err
	}
	removed := false
	defer func() {
		if !removed {
			finishChange(p)
		}
	}()
	if err := r.stopService(p); err != nil {
		return err
	}
	if err := os.RemoveAll(p.Dir); err != nil {
		return err
	}
	r.mu.Lock()
	delete(r.pkgs, name)
	if p.Manifest != nil {
		r.deps.Registry.Unregister(name)
	}
	r.mu.Unlock()
	removed = true
	return nil
}
func (r *Registry) SetDisabled(name string, disabled bool) error {
	unlock := r.lockPackage(name)
	defer unlock()
	p := r.Get(name)
	if p == nil {
		return fmt.Errorf("skill package %q not installed", name)
	}
	p.mu.Lock()
	p.changing = true
	p.mu.Unlock()
	defer finishChange(p)
	rec := p.Record()
	rec.Disabled = disabled
	if err := writeRecord(p.Dir, rec); err != nil {
		return err
	}
	p.mu.Lock()
	p.record = rec
	p.disabled = disabled
	p.mu.Unlock()
	if disabled {
		return r.stopService(p)
	}
	return nil
}

// Close stops and waits for all package services during host shutdown.
func (r *Registry) Close() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	for _, name := range r.List() {
		unlock := r.lockPackage(name)
		p := r.Get(name)
		if p != nil {
			p.mu.Lock()
			p.changing = true
			p.mu.Unlock()
			if err := r.stopService(p); err != nil {
				r.logf("%v", err)
			}
		}
		unlock()
	}
}

// ResolveStreamEndpoint resolves only <package>.<stream>, without global aliases.
func (r *Registry) ResolveStreamEndpoint(endpoint string) (pkgName, streamName string, ok bool) {
	name, stream, found := strings.Cut(endpoint, ".")
	if !found {
		return "", "", false
	}
	p := r.Get(name)
	if p == nil || p.Manifest == nil {
		return "", "", false
	}
	for _, declared := range p.Manifest.Streams {
		if stream == declared {
			return name, stream, true
		}
	}
	return "", "", false
}
