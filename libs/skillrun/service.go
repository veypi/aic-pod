package skillrun

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/aic-skills/sdk/go/skillproc"
	"github.com/veypi/vbox"
	"github.com/veypi/vsh/commands"
)

const socketEnv = "SKILLPROC_SOCKET"

type serviceInst struct {
	socket   string
	cancel   context.CancelFunc
	done     chan struct{}
	ready    chan struct{}
	err      error // published by closing ready
	stopping bool  // guarded by Package.mu
}

// A package owns one lazily started service. Every caller waits on the same
// readiness result; caller cancellation does not stop the shared service.
func (r *Registry) ensureService(ctx context.Context, pkg *Package) (*serviceInst, error) {
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return nil, fmt.Errorf("skill registry closed")
	}
	pkg.mu.Lock()
	if pkg.changing || pkg.disabled {
		pkg.mu.Unlock()
		return nil, fmt.Errorf("skill %s unavailable", pkg.Name)
	}
	inst := pkg.service
	if inst != nil {
		select {
		case <-inst.done:
			inst = nil
		default:
			if inst.stopping {
				pkg.mu.Unlock()
				return nil, fmt.Errorf("skill %s service stopping", pkg.Name)
			}
		}
	}
	if inst == nil {
		if err := os.MkdirAll(r.deps.RunDir, 0700); err != nil {
			pkg.mu.Unlock()
			return nil, err
		}
		socket := filepath.Join(r.deps.RunDir, pkg.Name+".sock")
		_ = os.Remove(socket)
		svcCtx, cancel := context.WithCancel(context.Background())
		inst = &serviceInst{socket: socket, cancel: cancel, done: make(chan struct{}), ready: make(chan struct{})}
		pkg.service = inst
		manifest := pkg.Manifest
		argv := append([]string{filepath.Join(pkg.Dir, filepath.FromSlash(manifest.Entry))}, manifest.Args...)
		go func(inst *serviceInst) {
			defer close(inst.done)
			logw := &logfWriter{prefix: "skill " + pkg.Name + ": ", logf: r.logf}
			_, err := r.deps.Manager.RunProcess(svcCtx, vbox.StartOptions{Workdir: pkg.Dir, Exec: argv, Env: []string{socketEnv + "=" + socket}, NoSandbox: true}, nil, logw, logw)
			if err != nil {
				r.logf("skill service %s: %v", pkg.Name, err)
			}
		}(inst)
		go waitServiceReady(inst)
	}
	pkg.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-inst.ready:
	}
	if inst.err != nil {
		return nil, inst.err
	}
	select {
	case <-inst.done:
		return nil, fmt.Errorf("skill %s service exited", pkg.Name)
	default:
		return inst, nil
	}
}
func waitServiceReady(inst *serviceInst) {
	defer close(inst.ready)
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		conn, err := net.DialTimeout("unix", inst.socket, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		select {
		case <-inst.done:
			inst.err = fmt.Errorf("skill service exited during startup")
			return
		case <-timer.C:
			inst.err = fmt.Errorf("skill service not ready in 5s: %w", err)
			inst.cancel()
			<-inst.done
			return
		case <-ticker.C:
		}
	}
}
func (r *Registry) stopService(pkg *Package) error {
	pkg.mu.Lock()
	inst := pkg.service
	if inst != nil {
		inst.stopping = true
	}
	pkg.mu.Unlock()
	if inst == nil {
		return nil
	}
	inst.cancel()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case <-inst.done:
	case <-timer.C:
		return fmt.Errorf("skill %s: service did not exit within 10s", pkg.Name)
	}
	<-inst.ready
	pkg.mu.Lock()
	if pkg.service == inst {
		pkg.service = nil
	}
	pkg.mu.Unlock()
	_ = os.Remove(inst.socket)
	return nil
}

// invokeService 根命令 → service provider 的一次调用：独立拨号 → invoke 帧
// （stdin 一次性负载）→ frame/exit/error；ctx 取消关闭该次连接。
func (r *Registry) invokeService(ctx context.Context, inv *commands.Invocation, pkg *Package) error {
	inst, err := r.ensureService(ctx, pkg)
	if err != nil {
		return commands.Exitf(inv, 1, "%s: %s", pkg.Name, err)
	}
	conn, err := skillproc.Dial("unix", inst.socket)
	if err != nil {
		return commands.Exitf(inv, 1, "%s: service dial: %s", pkg.Name, err)
	}
	defer conn.Close()
	// 每次调用独占连接；断开连接同时取消服务端调用，并解锁本地 Send/Recv。
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	var stdin []byte
	if inv.Stdin != nil {
		stdin, err = io.ReadAll(io.LimitReader(inv.Stdin, skillproc.MaxPayload+1))
		if err != nil {
			return commands.Exitf(inv, 1, "%s: read stdin: %s", pkg.Name, err)
		}
	}
	if len(stdin) > skillproc.MaxPayload {
		return commands.Exitf(inv, 1, "%s: stdin exceeds %d bytes", pkg.Name, skillproc.MaxPayload)
	}
	workdir := inv.Cwd
	if r.deps.Workdir != nil {
		workdir = r.deps.Workdir(workdir)
	}
	if err := conn.Send(skillproc.Header{Type: skillproc.TypeInvoke, Argv: inv.Args, Cwd: workdir, Env: proto.HostEnvMapToOS(inv.Env)}, stdin); err != nil {
		if ctx.Err() != nil {
			return commands.Exitf(inv, 124, "%s: %s", pkg.Name, ctx.Err())
		}
		return commands.Exitf(inv, 1, "%s: invoke: %s", pkg.Name, err)
	}
	for {
		h, payload, err := conn.Recv()
		if err != nil {
			if ctx.Err() != nil {
				return commands.Exitf(inv, 124, "%s: %s", pkg.Name, ctx.Err())
			}
			return commands.Exitf(inv, 1, "%s: service conn: %s", pkg.Name, err)
		}
		switch h.Type {
		case skillproc.TypeFrame:
			w := inv.Stdout
			if h.Stream == skillproc.StreamStderr {
				w = inv.Stderr
			}
			if w != nil {
				if _, err := w.Write(payload); err != nil {
					return err
				}
			}
		case skillproc.TypeExit:
			if h.Error != "" {
				return commands.Exitf(inv, exitCodeOr(h.Code, 1), "%s: %s", pkg.Name, h.Error)
			}
			if h.Code != 0 {
				return &commands.ExitError{Code: h.Code}
			}
			return nil
		case skillproc.TypeError:
			return commands.Exitf(inv, 1, "%s: %s", pkg.Name, h.Error)
		default:
			return commands.Exitf(inv, 1, "%s: unexpected frame %q", pkg.Name, h.Type)
		}
	}
}

// Stream owns one connection. Read/ReadFrame have one reader; Close is concurrent-safe.
type Stream struct {
	conn    *skillproc.Conn
	buf     bytes.Buffer
	once    sync.Once
	release func()
	stop    func() bool
}

func (r *Registry) OpenStream(ctx context.Context, pkgName, streamName string, args ...[]byte) (*Stream, error) {
	pkg, release, err := r.begin(pkgName)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			release()
		}
	}()
	if pkg.Manifest == nil || pkg.Manifest.Kind != KindService || !slices.Contains(pkg.Manifest.Streams, streamName) {
		return nil, fmt.Errorf("skill %s has no stream %q", pkgName, streamName)
	}
	inst, err := r.ensureService(ctx, pkg)
	if err != nil {
		return nil, err
	}
	conn, err := skillproc.Dial("unix", inst.socket)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	var payload []byte
	if len(args) > 0 {
		payload = args[0]
	}
	if err := conn.Send(skillproc.Header{Type: skillproc.TypeStreamOpen, Name: streamName}, payload); err != nil {
		stop()
		_ = conn.Close()
		return nil, err
	}
	success = true
	return &Stream{conn: conn, release: release, stop: stop}, nil
}
func (s *Stream) Write(p []byte) (int, error) {
	if err := s.conn.Send(skillproc.Header{Type: skillproc.TypeStreamFrame}, p); err != nil {
		return 0, err
	}
	return len(p), nil
}
func (s *Stream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for s.buf.Len() == 0 {
		b, err := s.ReadFrame()
		if err != nil {
			return 0, err
		}
		s.buf.Write(b)
	}
	return s.buf.Read(p)
}
func (s *Stream) ReadFrame() ([]byte, error) {
	h, p, err := s.conn.Recv()
	if err != nil {
		return nil, err
	}
	switch h.Type {
	case skillproc.TypeStreamFrame:
		return p, nil
	case skillproc.TypeStreamClose:
		if h.Error != "" {
			return nil, fmt.Errorf("stream: %s", h.Error)
		}
		return nil, io.EOF
	case skillproc.TypeError:
		return nil, fmt.Errorf("stream: %s", h.Error)
	default:
		return nil, fmt.Errorf("unexpected stream frame %q", h.Type)
	}
}
func (s *Stream) Close() error {
	s.once.Do(func() {
		if s.stop != nil {
			s.stop()
		}
		_ = s.conn.Close()
		if s.release != nil {
			s.release()
		}
	})
	return nil
}

// logfWriter provider stdio → logf（诊断流，不入协议）。
type logfWriter struct {
	prefix string
	logf   func(string, ...any)
}

func (w *logfWriter) Write(p []byte) (int, error) {
	if w.logf != nil {
		w.logf("%s%s", w.prefix, strings.TrimRight(string(p), "\n"))
	}
	return len(p), nil
}
