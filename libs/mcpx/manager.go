// Package mcpx connects configured MCP services. Skills and installation are
// deliberately outside this package.
//
// 连接生命周期：服务在首次调用时懒启动，之后复用同一 SDK session；
// 配置 idle_timeout 的服务在最后一次使用后空闲超时被回收（在途请求算使用），
// 下一次调用重新懒启动。
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
	// IdleTimeout 空闲有效期（如 "30m"）：服务启动或最后一次使用后经过该时长
	// 无任何调用即被回收，下次调用重新懒启动。空 = 永不过期（服务随 Pod 存活）。
	IdleTimeout string `json:"idle_timeout,omitempty" yaml:"idle_timeout,omitempty"`
}

// IdleDuration 返回解析后的空闲有效期：0 表示不过期。
func (c Config) IdleDuration() (time.Duration, error) {
	if c.IdleTimeout == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(c.IdleTimeout)
	if err != nil {
		return 0, fmt.Errorf("not a duration: %q", c.IdleTimeout)
	}
	if d <= 0 {
		return 0, fmt.Errorf("must be positive: %q", c.IdleTimeout)
	}
	return d, nil
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
	if _, err := c.IdleDuration(); err != nil {
		return fmt.Errorf("mcp %s: invalid idle_timeout: %w", alias, err)
	}
	return nil
}

type Options struct {
	Authorize  func(context.Context, string, string, mcp.Params) error
	Processes  *vbox.Manager
	Policy     func(Config) vbox.Policy
	HTTPClient *http.Client
	Logf       func(string, ...any)
	// Expired 在服务因空闲有效期被回收后调用（进程已结束）。调用方据此释放
	// 服务进程之外的资源（如内置 browser 的常驻 daemon）。
	Expired func(name string)
}

type instance struct {
	ready    chan struct{}
	done     chan struct{}
	cancel   context.CancelFunc
	session  *mcp.ClientSession
	err      error
	idle     time.Duration // 0 = 不过期
	used     time.Time     // 最后一次使用（取到 session 或在途请求结束）
	inflight int           // 在途请求数：>0 时不回收
}

type Manager struct {
	mu        sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
	configs   map[string]Config
	instances map[string]*instance
	launched  map[string]bool // 曾经启动过（Started；空闲回收不撤销）
	options   Options
	closed    bool
	reapEvery time.Duration // 空闲扫描周期，0 = 无服务配置有效期
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
	m := &Manager{ctx: ctx, cancel: cancel, configs: copy, instances: map[string]*instance{}, launched: map[string]bool{}, options: options, reapEvery: reapEvery(copy)}
	if m.reapEvery > 0 {
		go m.reap()
	}
	return m, nil
}

// reapEvery 返回空闲扫描周期：最短有效期的四分之一，钳在 0.25s..30s（有效期为
// 分钟级时即“到期后半个扫描周期内回收”）；没有任何配置有效期时返回 0（不扫描）。
func reapEvery(configs map[string]Config) time.Duration {
	shortest := time.Duration(0)
	for _, cfg := range configs {
		d, err := cfg.IdleDuration()
		if err != nil || d <= 0 {
			continue
		}
		if shortest == 0 || d < shortest {
			shortest = d
		}
	}
	if shortest == 0 {
		return 0
	}
	every := shortest / 4
	if every < 250*time.Millisecond {
		every = 250 * time.Millisecond
	}
	if every > 30*time.Second {
		every = 30 * time.Second
	}
	return every
}

// Session shares a device-owned connection. The request context only controls
// waiting for readiness; it never owns the service process or its environment.
// 每次取到连接都刷新空闲截止时间，因此调用即续期。
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
		idle, _ := cfg.IdleDuration()
		i = &instance{ready: make(chan struct{}), done: make(chan struct{}), cancel: cancel, idle: idle, used: time.Now()}
		m.instances[name] = i
		m.launched[name] = true
		go m.connect(lifetime, name, cfg, i)
	} else {
		i.used = time.Now()
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
	// 每个入站请求都在共享连接上留一次活动痕迹：空闲回收据此判定，长调用
	// 在途时不会被误回收。授权门保持不变。
	client.AddSendingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			m.beginRequest(i)
			defer m.endRequest(i)
			if m.options.Authorize != nil {
				switch method {
				case "tools/list", "tools/call", "resources/list", "resources/templates/list", "resources/read":
					if err := m.options.Authorize(ctx, name, method, req.GetParams()); err != nil {
						return nil, err
					}
				}
			}
			return next(ctx, method, req)
		}
	})
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
		// 主动取消（Close/Restart）时连接随进程一起收尾，Wait 报错是噪声。
		if ctx.Err() == nil {
			m.options.Logf("mcp %s: connection ended: %v", name, err)
		}
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
		} else if procCtx.Err() == nil && code != 0 {
			// 主动取消（Close/Restart）后退出码无意义（-1）：只在服务自行退出时记录。
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

// Started reports whether this manager ever launched the named service. Idle
// release forgets the running connection but not the launch history.
func (m *Manager) Started(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.launched[name]
}

// Touch refreshes the idle deadline of a started service. Callers that hold the
// service's upstream resources without issuing MCP requests (UI live views) use
// it to keep the service alive for as long as the attachment lasts; a service
// that was never started is not started by Touch.
func (m *Manager) Touch(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i := m.instances[name]; i != nil {
		select {
		case <-i.done:
		default:
			i.used = time.Now()
		}
	}
}

func (m *Manager) beginRequest(i *instance) {
	m.mu.Lock()
	i.inflight++
	m.mu.Unlock()
}

func (m *Manager) endRequest(i *instance) {
	m.mu.Lock()
	if i.inflight > 0 {
		i.inflight--
	}
	i.used = time.Now()
	m.mu.Unlock()
}

// reap stops services that have been idle beyond their configured timeout.
func (m *Manager) reap() {
	ticker := time.NewTicker(m.reapEvery)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case now := <-ticker.C:
			type released struct {
				name string
				i    *instance
			}
			var expired []released
			m.mu.Lock()
			for name, i := range m.instances {
				if i.idle <= 0 || i.inflight > 0 || now.Sub(i.used) < i.idle {
					continue
				}
				select {
				case <-i.done: // 连接已自行结束，Session 会替换
					continue
				default:
				}
				// 先从表里摘除：期间的新调用直接建新连接，不会拿到将被关闭的 session。
				delete(m.instances, name)
				expired = append(expired, released{name: name, i: i})
			}
			m.mu.Unlock()
			for _, r := range expired {
				m.expire(r.name, r.i)
			}
		}
	}
}

// expire 结束一个已摘除的空闲实例：主动取消属控制行为，连接结束不记故障
// 日志；回收本身记一条可见的生命周期日志。
func (m *Manager) expire(name string, i *instance) {
	if i == nil {
		return
	}
	i.cancel()
	m.options.Logf("mcp %s: idle timeout reached; service released after %s", name, i.idle)
	if m.options.Expired != nil {
		m.options.Expired(name)
	}
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
