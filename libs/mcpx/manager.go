// Package mcpx connects configured MCP services. Skills and installation are
// deliberately outside this package.
package mcpx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/veypi/vbox"
)

type Config struct {
	Command   string            `json:"command,omitempty" yaml:"command,omitempty"`
	Args      []string          `json:"args,omitempty" yaml:"args,omitempty"`
	Cwd       string            `json:"cwd,omitempty" yaml:"cwd,omitempty"`
	Env       map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	URL       string            `json:"url,omitempty" yaml:"url,omitempty"`
	Disabled  bool              `json:"disabled,omitempty" yaml:"disabled,omitempty"`
	NoSandbox bool              `json:"no_sandbox,omitempty" yaml:"no_sandbox,omitempty"`
}

type Settings struct {
	Servers map[string]Config `json:"servers,omitempty" yaml:"servers,omitempty"`
}

var aliasPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

func (c Config) Validate(alias string) error {
	if !aliasPattern.MatchString(alias) {
		return fmt.Errorf("mcp: invalid server alias %q", alias)
	}
	if c.Disabled {
		return nil
	}
	if (c.Command == "") == (c.URL == "") {
		return fmt.Errorf("mcp %s: configure exactly one of command or url", alias)
	}
	if c.Cwd != "" && !filepath.IsAbs(c.Cwd) {
		return fmt.Errorf("mcp %s: cwd must be absolute", alias)
	}
	if c.URL != "" {
		u, err := url.Parse(c.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "" {
			return fmt.Errorf("mcp %s: invalid HTTP endpoint", alias)
		}
		if len(c.Args) != 0 || len(c.Env) != 0 || c.Cwd != "" || c.NoSandbox {
			return fmt.Errorf("mcp %s: process settings require command", alias)
		}
	}
	for key, value := range c.Env {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, 0) {
			return fmt.Errorf("mcp %s: invalid environment", alias)
		}
	}
	return nil
}

type Options struct {
	Authorize  func(context.Context, string, string, mcp.Params) error
	Processes  *vbox.Manager
	Policy     func(Config) vbox.Policy
	HTTPClient *http.Client
	Logf       func(string, ...any)
}

type instance struct {
	ready   chan struct{}
	done    chan struct{}
	cancel  context.CancelFunc
	session *mcp.ClientSession
	err     error
}

type Manager struct {
	mu        sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
	configs   map[string]Config
	instances map[string]*instance
	options   Options
	closed    bool
}

func NewManager(configs map[string]Config, options Options) (*Manager, error) {
	copy := make(map[string]Config, len(configs))
	for name, cfg := range configs {
		if err := cfg.Validate(name); err != nil {
			return nil, err
		}
		cfg.Args = append([]string(nil), cfg.Args...)
		env := make(map[string]string, len(cfg.Env))
		for k, v := range cfg.Env {
			env[k] = v
		}
		cfg.Env = env
		copy[name] = cfg
	}
	if options.Processes == nil {
		return nil, errors.New("mcp: process manager required")
	}
	if options.Logf == nil {
		options.Logf = func(string, ...any) {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{ctx: ctx, cancel: cancel, configs: copy, instances: map[string]*instance{}, options: options}, nil
}

// Session shares a device-owned connection. The request context only controls
// waiting for readiness; it never owns the service process or its environment.
func (m *Manager) Session(ctx context.Context, name string) (*mcp.ClientSession, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("mcp: manager closed")
	}
	cfg, ok := m.configs[name]
	if !ok || cfg.Disabled {
		m.mu.Unlock()
		return nil, fmt.Errorf("mcp %s: service is not configured or is disabled", name)
	}
	i := m.instances[name]
	if i != nil {
		select {
		case <-i.done:
			i = nil
		default:
		}
	}
	if i == nil {
		lifetime, cancel := context.WithCancel(m.ctx)
		i = &instance{ready: make(chan struct{}), done: make(chan struct{}), cancel: cancel}
		m.instances[name] = i
		go m.connect(lifetime, name, cfg, i)
	}
	m.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-i.ready:
	}
	if i.err != nil {
		return nil, i.err
	}
	select {
	case <-i.done:
		return nil, fmt.Errorf("mcp %s: connection closed", name)
	default:
		return i.session, nil
	}
}

func (m *Manager) connect(ctx context.Context, name string, cfg Config, i *instance) {
	defer close(i.done)
	defer i.cancel()
	transport, stop, err := m.transport(ctx, name, cfg)
	if err != nil {
		i.err = err
		close(i.ready)
		return
	}
	defer stop()
	startup, cancel := context.WithTimeout(ctx, 20*time.Second)
	client := mcp.NewClient(&mcp.Implementation{Name: "aic", Version: "1"}, nil)
	if m.options.Authorize != nil {
		client.AddSendingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
			return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
				switch method {
				case "tools/list", "tools/call", "resources/list", "resources/templates/list", "resources/read":
					if err := m.options.Authorize(ctx, name, method, req.GetParams()); err != nil {
						return nil, err
					}
				}
				return next(ctx, method, req)
			}
		})
	}
	session, err := client.Connect(startup, transport, nil)
	cancel()
	if err != nil {
		i.err = fmt.Errorf("mcp %s: connect: %w", name, err)
		close(i.ready)
		return
	}
	i.session = session
	close(i.ready)
	stopClose := context.AfterFunc(ctx, func() { _ = session.Close() })
	defer stopClose()
	if err := session.Wait(); err != nil {
		m.options.Logf("mcp %s: connection ended: %v", name, err)
	}
}

func (m *Manager) transport(ctx context.Context, name string, cfg Config) (mcp.Transport, func(), error) {
	if cfg.URL != "" {
		return &mcp.StreamableClientTransport{Endpoint: cfg.URL, HTTPClient: m.options.HTTPClient}, func() {}, nil
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, nil, err
	}
	procCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	env := make([]string, 0, len(cfg.Env))
	for k, v := range cfg.Env {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)
	opts := vbox.StartOptions{Exec: append([]string{cfg.Command}, cfg.Args...), Workdir: cfg.Cwd, Env: env, NoSandbox: cfg.NoSandbox, RawOutput: true}
	if m.options.Policy != nil {
		opts.Policy = m.options.Policy(cfg)
	}
	go func() {
		defer close(done)
		code, err := m.options.Processes.RunProcess(procCtx, opts, inR, outW, &logWriter{name: name, logf: m.options.Logf})
		_ = outW.Close()
		_ = inR.Close()
		if err != nil && procCtx.Err() == nil {
			m.options.Logf("mcp %s: process failed: %v", name, err)
		} else if code != 0 {
			m.options.Logf("mcp %s: process exited with %d", name, code)
		}
	}()
	return &mcp.IOTransport{Reader: outR, Writer: inW}, func() {
		cancel()
		_ = inW.Close()
		_ = outR.Close()
		// The process owns its pipe ends until Start/Wait have returned.
		<-done
	}, nil
}

type logWriter struct {
	name string
	logf func(string, ...any)
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.logf("mcp %s: %s", w.name, strings.TrimSpace(string(p)))
	return len(p), nil
}

// Names contains no credentials or launch environment.
func (m *Manager) Names() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.configs))
	for k, c := range m.configs {
		if !c.Disabled {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	return names
}

func (m *Manager) Restart(ctx context.Context, name string) error {
	m.mu.Lock()
	i := m.instances[name]
	_, exists := m.configs[name]
	m.mu.Unlock()
	if !exists {
		return fmt.Errorf("mcp %s: unknown service", name)
	}
	if i == nil {
		return nil
	}
	i.cancel()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-i.done:
		return nil
	}
}

// Started reports whether this manager ever launched the named service.
func (m *Manager) Started(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.instances[name] != nil
}

func (m *Manager) Close() error {
	m.mu.Lock()
	m.closed = true
	all := make([]*instance, 0, len(m.instances))
	for _, i := range m.instances {
		all = append(all, i)
	}
	m.mu.Unlock()
	m.cancel()
	for _, i := range all {
		<-i.done
	}
	return nil
}

var _ io.Writer = (*logWriter)(nil)
