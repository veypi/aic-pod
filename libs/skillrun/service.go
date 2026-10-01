package skillrun

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/veypi/vbox"
	"github.com/veypi/vsh/commands"

	vshglue "github.com/veypi/aic-pod/libs/vsh"
	"github.com/veypi/aic-pod/protocol/skillproc"
)

// service.go service 类 provider 的运行时（aic/docs/skill.md §9.2）：
// 首次调用懒启动（vbox 沙箱驻留进程）→ 登记 bg 任务表（bg list/kill 管理，
// 崩溃下次调用重拉，无独立 supervisor）→ 调用经 skillproc 最小协议走本地
// socket（SKILLPROC_SOCKET 经 env 注入 provider）。invoke/stream 每路独立
// 拨号（无握手，demux 归零）；卸载 = 解注册 + bg kill（本结构 cancel）。

// socketEnv service provider 的 socket 路径注入键（vbox StartOptions.Env
// 下发，沙箱清洗后应用）。
const socketEnv = "SKILLPROC_SOCKET"

// serviceInst 是一个驻留 service provider 实例。
type serviceInst struct {
	socket string
	cancel context.CancelFunc
	done   chan struct{} // 进程退出即关闭（崩溃探测 → 下次调用重拉）
	taskID string        // bg 任务表登记 ID
}

// svcKey 实例键：包内 provider 唯一。
func svcKey(pkgName, providerID string) string { return pkgName + "/" + providerID }

// ensureService 取驻留实例（不存在/已崩溃 = 懒启动）。bg 登记失败（容量满/
// 进程即死）如实报错——bg 是管理面，与 exec 同一容量语义。
func (r *Registry) ensureService(ctx context.Context, pkg *Package, p Provider) (*serviceInst, error) {
	key := svcKey(pkg.Name, p.ID)
	r.mu.Lock()
	if s, ok := r.svcs[key]; ok {
		select {
		case <-s.done: // 已崩溃：下次调用重拉
			delete(r.svcs, key)
		default:
			r.mu.Unlock()
			return s, nil
		}
	}
	socket := filepath.Join(r.deps.RunDir, pkg.Name+"-"+p.ID+".sock")
	if err := os.MkdirAll(r.deps.RunDir, 0o700); err != nil {
		r.mu.Unlock()
		return nil, fmt.Errorf("skillrun: run dir: %w", err)
	}
	_ = os.Remove(socket) // 陈旧 socket（上次崩溃残留）
	svcCtx, cancel := context.WithCancel(context.Background())
	entry := filepath.Join(pkg.Dir, filepath.FromSlash(p.Entry))
	argv := append([]string{entry}, p.Args...)
	// service 沙箱姿态（2026-10-02 用户定，P5 真机回归）：一律 NoSandbox——
	// 驻留 GUI 服务保持其实际原生权限边界（design.md §9.3：不声称每会话文件
	// 隔离）。会话规则是给 AI 调用戴的（如 .aic/browser deny 护 cookie），套给
	// svc 等于浏览器自断手脚（StateDir/socket 全在会话 deny/未放行面内）；
	// 信任语义 = 安装 service 包即设备级信任，与 v5 进程内形态等价。process
	// 类 provider 维持每调用按调用身份算沙箱规则集，不受影响。
	inst := &serviceInst{socket: socket, cancel: cancel, done: make(chan struct{})}
	h := vshglue.NewExecHandle(cancel)
	logw := &logfWriter{prefix: "skill " + key + ": ", logf: r.logf}
	go func() {
		code, err := r.deps.Manager.RunProcess(svcCtx, vbox.StartOptions{
			Workdir:   pkg.Dir,
			Exec:      argv,
			Env:       []string{socketEnv + "=" + socket},
			NoSandbox: true,
		}, nil, logw, logw)
		h.Finish(&vshglue.ExecResult{ExitCode: code}, err)
		close(inst.done)
	}()
	if r.deps.Tasks != nil {
		tasks, err := r.deps.Tasks()
		if err != nil {
			cancel()
			r.mu.Unlock()
			return nil, fmt.Errorf("skillrun: task table: %w", err)
		}
		t, err := tasks.Adopt(h, "skill service "+key, vshglue.TaskMeta{
			Owner:   vshglue.OwnerFromContext(ctx),
			Session: vshglue.SessionFromContext(ctx),
		})
		if err != nil {
			cancel()
			r.mu.Unlock()
			return nil, fmt.Errorf("skillrun: bg adopt: %w", err)
		}
		inst.taskID = t.ID
	}
	r.svcs[key] = inst
	r.mu.Unlock()
	// 等 provider 监听就绪（进程即死 = 如实报错，含启动失败）。
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := skillproc.Dial("unix", socket)
		if err == nil {
			c.Close()
			r.logf("skillrun: service %s up (task %s)", key, inst.taskID)
			return inst, nil
		}
		select {
		case <-inst.done:
			return nil, fmt.Errorf("skillrun: service %s exited during startup", key)
		default:
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("skillrun: service %s not ready in 5s: %w", key, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// invokeService 根命令 → service provider 的一次调用：独立拨号 → invoke 帧
// （stdin 一次性负载）→ demux frame/exit/error；ctx 取消转发 cancel 帧。
func (r *Registry) invokeService(ctx context.Context, inv *commands.Invocation, pkg *Package, p Provider) error {
	inst, err := r.ensureService(ctx, pkg, p)
	if err != nil {
		return commands.Exitf(inv, 1, "%s: %s", pkg.Name, err)
	}
	conn, err := skillproc.Dial("unix", inst.socket)
	if err != nil {
		return commands.Exitf(inv, 1, "%s: service dial: %s", pkg.Name, err)
	}
	defer conn.Close()
	var stdin []byte
	if inv.Stdin != nil {
		stdin, err = io.ReadAll(io.LimitReader(inv.Stdin, skillproc.MaxPayload))
		if err != nil {
			return commands.Exitf(inv, 1, "%s: read stdin: %s", pkg.Name, err)
		}
	}
	id := fmt.Sprintf("i-%d", atomic.AddInt64(&r.seq, 1))
	if err := conn.Send(skillproc.Header{ID: id, Type: skillproc.TypeInvoke, Argv: inv.Args, Cwd: inv.Cwd}, stdin); err != nil {
		return commands.Exitf(inv, 1, "%s: invoke: %s", pkg.Name, err)
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Send(skillproc.Header{ID: id, Type: skillproc.TypeCancel}, nil)
		case <-stop:
		}
	}()
	for {
		h, payload, err := conn.Recv()
		if err != nil {
			return commands.Exitf(inv, 1, "%s: service conn: %s", pkg.Name, err)
		}
		if h.ID != id {
			continue
		}
		switch h.Type {
		case skillproc.TypeFrame:
			w := inv.Stdout
			if h.Stream == skillproc.StreamStderr {
				w = inv.Stderr
			}
			if w != nil {
				_, _ = w.Write(payload)
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
		}
	}
}

// Stream 是一条已打开的 stream 二进制通道（每通道独立连接，单读者单写者）。
type Stream struct {
	conn *skillproc.Conn
	id   string
	buf  bytes.Buffer
	eof  bool
}

// OpenStream 打开包声明的 stream 端点（manifest streams[]，按名路由到
// provider，懒启动）。args = stream.open 帧负载（端点打开参数，如 browser 的
// PageArgs JSON；缺省无参数）。禁用包显式失败（与根命令同语义）；非 CLI 包
// （Manifest=nil）无 stream 声明。
func (r *Registry) OpenStream(ctx context.Context, pkgName, streamName string, args ...[]byte) (*Stream, error) {
	pkg := r.Get(pkgName)
	if pkg == nil {
		return nil, fmt.Errorf("skillrun: package %q not installed", pkgName)
	}
	if pkg.Disabled() {
		return nil, fmt.Errorf("skillrun: package %q disabled（skill 包已禁用）", pkgName)
	}
	if pkg.Manifest == nil {
		return nil, fmt.Errorf("skillrun: package %q has no cli manifest（非 CLI 资源包无 stream）", pkgName)
	}
	var decl *StreamDecl
	for i := range pkg.Manifest.Streams {
		if pkg.Manifest.Streams[i].Name == streamName {
			decl = &pkg.Manifest.Streams[i]
			break
		}
	}
	if decl == nil {
		return nil, fmt.Errorf("skillrun: package %q has no stream %q", pkgName, streamName)
	}
	var provider *Provider
	for i := range pkg.Manifest.Providers {
		if pkg.Manifest.Providers[i].ID == decl.Provider {
			provider = &pkg.Manifest.Providers[i]
			break
		}
	}
	if provider == nil || provider.Kind != KindService {
		return nil, fmt.Errorf("skillrun: stream %q provider %q is not a service", streamName, decl.Provider)
	}
	inst, err := r.ensureService(ctx, pkg, *provider)
	if err != nil {
		return nil, err
	}
	conn, err := skillproc.Dial("unix", inst.socket)
	if err != nil {
		return nil, fmt.Errorf("skillrun: service dial: %w", err)
	}
	id := fmt.Sprintf("s-%d", atomic.AddInt64(&r.seq, 1))
	var payload []byte
	if len(args) > 0 {
		payload = args[0]
	}
	if err := conn.Send(skillproc.Header{ID: id, Type: skillproc.TypeStreamOpen, Name: streamName}, payload); err != nil {
		conn.Close()
		return nil, err
	}
	return &Stream{conn: conn, id: id}, nil
}

// Write 发一帧（不透明字节；底层连接写端串行化）。
func (s *Stream) Write(p []byte) (int, error) {
	if s.eof {
		return 0, io.ErrClosedPipe
	}
	if err := s.conn.Send(skillproc.Header{ID: s.id, Type: skillproc.TypeStreamFrame}, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Read 收一帧（单读者）。
func (s *Stream) Read(p []byte) (int, error) {
	for s.buf.Len() == 0 && !s.eof {
		h, payload, err := s.conn.Recv()
		if err != nil {
			return 0, err
		}
		if h.ID != s.id {
			continue
		}
		switch h.Type {
		case skillproc.TypeStreamFrame:
			s.buf.Write(payload)
		case skillproc.TypeStreamClose:
			s.eof = true
		case skillproc.TypeError:
			return 0, fmt.Errorf("stream error: %s", h.Error)
		}
	}
	if s.buf.Len() == 0 {
		return 0, io.EOF
	}
	return s.buf.Read(p)
}

// ReadFrame 收一条 stream 帧（消息边界保持——RTC 桥接用；io 字节流读者用
// Read）。io.EOF = 对端关闭。
func (s *Stream) ReadFrame() ([]byte, error) {
	for {
		h, payload, err := s.conn.Recv()
		if err != nil {
			return nil, err
		}
		if h.ID != s.id {
			continue
		}
		switch h.Type {
		case skillproc.TypeStreamFrame:
			return payload, nil
		case skillproc.TypeStreamClose:
			s.eof = true
			return nil, io.EOF
		case skillproc.TypeError:
			return nil, fmt.Errorf("stream error: %s", h.Error)
		}
	}
}

// Close 关闭通道（关闭流就是关闭通道）。
func (s *Stream) Close() error {
	if !s.eof {
		_ = s.conn.Send(skillproc.Header{ID: s.id, Type: skillproc.TypeStreamClose}, nil)
		s.eof = true
	}
	return s.conn.Close()
}

// killServicesLocked 停包全部驻留 provider（卸载路径；进程组经 ctx 取消
// 受管杀，bg 任务表由句柄终结自动结算）。
func (r *Registry) killServicesLocked(pkgName string) {
	prefix := pkgName + "/"
	for key, inst := range r.svcs {
		if strings.HasPrefix(key, prefix) {
			inst.cancel()
			delete(r.svcs, key)
		}
	}
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
