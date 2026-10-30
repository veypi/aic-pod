package host

import (
	"context"
	"fmt"
	"io"
	"net"
	"os/exec"
	"sort"
	"sync"
	"time"

	"github.com/veypi/aic-pod/protocol"
	"github.com/veypi/vbox"
	"github.com/veypi/vsh/commands"
)

// streamWriteTimeout 是命令字节流中继写设备侧的写超时。
//
// 流的维护归 pod：写不动（设备进程卡死或不再消费）就断开这条流，
// 设备侧摘掉订阅者，页面重新活跃时自己新建流。
const streamWriteTimeout = 30 * time.Second

func (c *Client) runHostCommand(ctx context.Context, def *hostCommand, inv *commands.Invocation) error {
	wd := def.cwd()
	if wd == "" {
		wd = c.options().WorkDir
	}
	env := HostEnvToOS(inv.Env)
	keys := make([]string, 0, len(def.Config.Env))
	for key := range def.Config.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		env = append(env, key+"="+def.Config.Env[key])
	}
	argv := append(append([]string(nil), def.Config.Run...), inv.Args...)
	code, err := c.procs.RunProcess(ctx, vbox.StartOptions{
		Exec: argv, Workdir: wd, Env: env, ReplaceEnv: true, RawOutput: true,
		NoSandbox: def.noSandbox(), Policy: c.nativePolicy(ctx, wd, def.Config.Run[0]),
	}, inv.Stdin, inv.Stdout, inv.Stderr)
	if err != nil {
		return commands.Exitf(inv, 126, "%s: %v", def.Alias, err)
	}
	if code != 0 {
		return &commands.ExitError{Code: code}
	}
	return nil
}

type streamProcess struct {
	client *Client
	def    *hostCommand

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	done    chan struct{}
	active  int
	timer   *time.Timer
}

func newStreamProcess(client *Client, def *hostCommand) *streamProcess {
	return &streamProcess{client: client, def: def}
}

func (p *streamProcess) start() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running {
		return nil
	}
	if len(p.def.Config.Run) == 0 {
		return fmt.Errorf("%s: run is empty", p.def.Alias)
	}
	if _, err := exec.LookPath(p.def.Config.Run[0]); err != nil {
		return fmt.Errorf("%s: not built or program unavailable: %w", p.def.Alias, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.running, p.cancel, p.done = true, cancel, make(chan struct{})
	p.armIdleLocked()
	go p.run(ctx, p.done)
	return nil
}

func (p *streamProcess) run(ctx context.Context, done chan struct{}) {
	env := make([]string, 0, len(p.def.Config.Env)+len(p.def.Streams))
	for key, value := range p.def.environment() {
		env = append(env, key+"="+value)
	}
	sort.Strings(env)
	code, err := p.client.procs.RunProcess(ctx, vbox.StartOptions{
		Exec: p.def.Config.Run, Workdir: p.def.cwd(), Env: env, RawOutput: true,
		NoSandbox: p.def.noSandbox(), Policy: p.client.nativePolicy(context.Background(), p.def.cwd(), p.def.Config.Run[0]),
	}, nil, io.Discard, io.Discard)
	p.mu.Lock()
	p.running = false
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	close(done)
	p.mu.Unlock()
	if err != nil && ctx.Err() == nil {
		p.client.logf("stream command %s exited: %v", p.def.Alias, err)
	} else if code != 0 && ctx.Err() == nil {
		p.client.logf("stream command %s exited with code %d", p.def.Alias, code)
	}
}

func (p *streamProcess) armIdleLocked() {
	if p.def.Config.IdleTimeout == "" {
		return
	}
	idle, err := p.def.mcpConfig().IdleDuration()
	if err != nil || idle <= 0 {
		return
	}
	if p.timer == nil {
		p.timer = time.AfterFunc(idle, func() {
			p.mu.Lock()
			idle := p.active == 0
			p.mu.Unlock()
			if idle {
				p.stop()
			}
		})
		return
	}
	p.timer.Reset(idle)
}

func (p *streamProcess) touch() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running {
		p.armIdleLocked()
	}
}

func (p *streamProcess) attach() error {
	if err := p.start(); err != nil {
		return err
	}
	p.mu.Lock()
	p.active++
	p.armIdleLocked()
	p.mu.Unlock()
	return nil
}

func (p *streamProcess) detach() {
	p.mu.Lock()
	if p.active > 0 {
		p.active--
	}
	p.armIdleLocked()
	p.mu.Unlock()
}

func (p *streamProcess) stop() {
	p.mu.Lock()
	if !p.running {
		p.mu.Unlock()
		return
	}
	cancel, done := p.cancel, p.done
	p.mu.Unlock()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		p.client.logf("stream command %s did not stop within 5s", p.def.Alias)
	}
}

func (c *Client) streamHostCommand(parent context.Context, caller protocol.Caller, binding commandStreamBinding, input <-chan []byte, send func([]byte) error) error {
	def := binding.def
	check := func() error {
		if err := caller.Validate(parent); err != nil {
			return err
		}
		if !caller.Direct || caller.Scope != "" || !c.execAllowed(caller.Origin, def.Alias) {
			return protocol.Fail("permission_denied", "Command stream denied; request grant cmd "+def.Alias)
		}
		return nil
	}
	if err := check(); err != nil {
		return err
	}
	if binding.manager != nil && def.Config.Protocol == "mcp" {
		if _, err := binding.manager.Session(parent, def.Alias); err != nil {
			return err
		}
	}
	if binding.process != nil {
		if err := binding.process.attach(); err != nil {
			return err
		}
		defer binding.process.detach()
	}

	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var conn net.Conn
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	for {
		dialer := net.Dialer{Timeout: 500 * time.Millisecond}
		var err error
		conn, err = dialer.DialContext(ctx, "tcp", binding.stream.Address)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("%s stream %s: connect timeout: %w", def.Alias, binding.stream.Name, err)
		case <-time.After(50 * time.Millisecond):
		}
	}
	defer conn.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()

	touch := func() {
		if binding.manager != nil && def.Config.Protocol == "mcp" {
			binding.manager.Touch(def.Alias)
		}
		if binding.process != nil {
			binding.process.touch()
		}
	}
	touch()
	writes := make(chan error, 1)
	go func() {
		var err error
		defer func() {
			writes <- err
			cancel()
			_ = conn.Close() // 唤醒阻塞中的 Read；输入结束/写失败都能收口
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case raw, ok := <-input:
				if !ok {
					return
				}
				if err = check(); err != nil {
					return
				}
				for len(raw) > 0 {
					// 写超时即断流：流的维护归 pod——设备侧不读了（进程卡死、
					// 对端不再消费）就不能一直挂着。超时就关掉连接，设备侧会
					// 摘掉这个订阅者，页面重新活跃时自己新建流。
					_ = conn.SetWriteDeadline(time.Now().Add(streamWriteTimeout))
					var n int
					n, err = conn.Write(raw)
					if err != nil {
						return
					}
					if n <= 0 {
						err = io.ErrShortWrite
						return
					}
					raw = raw[n:]
				}
				touch()
			}
		}
	}()

	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				touch()
			}
		}
	}()

	buf := make([]byte, 64<<10)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			if checkErr := check(); checkErr != nil {
				return checkErr
			}
			touch()
			if sendErr := send(buf[:n]); sendErr != nil {
				return sendErr
			}
		}
		if err != nil {
			select {
			case writeErr := <-writes:
				if writeErr != nil {
					return writeErr
				}
			default:
			}
			if err == io.EOF || ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}
