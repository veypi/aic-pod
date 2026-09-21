package cua

// Native driver transport. ui/1 command semantics live in cua_ui.go.
// One persistent authenticated MCP connection preserves native snapshot handles.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// cuaMcpArgs 返回 mcp 子进程参数：darwin = daemon 唯一形态（socket 代理，
// daemon 拉起/就绪由 ensureCuaDaemon 保证）；非 darwin 无 CuaDriver.app
// daemon 自动拉起环境，保留 --direct 过渡。
func cuaMcpArgs(goos, sock string) []string {
	if goos == "darwin" {
		return []string{"mcp", "--socket", sock}
	}
	return []string{"mcp", "--direct"}
}

// cuaDaemonSocketPath 返回平台默认 daemon socket 路径（对齐驱动
// serve::default_socket_path()；macOS = ~/Library/Caches/cua-driver/cua-driver.sock）。
// 非 darwin 暂不支持 daemon 探测，返回空串（回退 --direct）。
func cuaDaemonSocketPath() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Caches", "cua-driver", "cua-driver.sock")
}

// cuaDaemonAlive 以 unix dial 探测 daemon 存活（500ms 超时）；仅看文件存在
// 会被 stale socket 卡住（ensureCuaDaemon 需要真实就绪判断）。
func cuaDaemonAlive(sock string) bool {
	if sock == "" {
		return false
	}
	c, err := net.DialTimeout("unix", sock, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// ---- daemon 自动拉起（darwin：CuaDriver.app 唯一形态） ----

const (
	cuaDaemonReadyTimeout = 15 * time.Second       // 拉起后等待 socket 就绪上限
	cuaDaemonPollInterval = 250 * time.Millisecond // socket 就绪轮询间隔
	cuaDaemonRetryAfter   = 30 * time.Second       // 拉起失败退避窗口（防调用风暴反复 open）
)

var cuaDaemonLaunchMu sync.Mutex
var cuaDaemonLastTry time.Time

// cuaDaemonLaunchArgs 返回拉起 daemon 的 open 参数（纯函数，可测）：
// -n 新实例、-g 不激活前台、--args serve 进入 daemon 形态；
// 原生窗口能力不申请浏览器 existing-profile grant。
// appPath 非空 = 桌面端内置发行物形态（resources/cua/darwin/CuaDriver.app，
// 路径直启，不依赖 LaunchServices 名称注册）；空 = 用户自装形态（-a CuaDriver）。
func cuaDaemonLaunchArgs(appPath string) []string {
	args := []string{"-n", "-g"}
	if appPath != "" {
		args = append(args, appPath)
	} else {
		args = append(args, "-a", "CuaDriver")
	}
	return append(args, "--args", "serve")
}

// ensureCuaDaemon 保证 darwin daemon 在监听：socket 存活直接返回；缺席则
// 拉起 CuaDriver.app 并轮询 socket 就绪。失败返回带指引的错误——永不静默
// 换身份。退避窗口内不重复拉起（防调用风暴）。
func ensureCuaDaemon(ctx context.Context, logf func(string, ...any)) error {
	sock := cuaDaemonSocketPath()
	if sock == "" {
		return fmt.Errorf("cua: 无法确定 daemon socket 路径（HOME 未设置？）")
	}
	if cuaDaemonAlive(sock) {
		return nil
	}
	cuaDaemonLaunchMu.Lock()
	if !cuaDaemonLastTry.IsZero() && time.Since(cuaDaemonLastTry) < cuaDaemonRetryAfter {
		cuaDaemonLaunchMu.Unlock()
		return fmt.Errorf("cua: CuaDriver daemon 未就绪（%.0fs 内已尝试拉起，退避中）；`cua-driver doctor` 诊断或手动 `open -g -a CuaDriver --args serve`",
			cuaDaemonRetryAfter.Seconds())
	}
	cuaDaemonLastTry = time.Now()
	cuaDaemonLaunchMu.Unlock()

	// CUA_DRIVER_APP：桌面端内置 CuaDriver.app 路径（resources/cua/darwin/）——
	// 路径直启（签名/公证原样保留，TCC 授权仍归 com.trycua.driver）；
	// 未设置时回落用户自装形态（open -a CuaDriver）。
	appPath := os.Getenv("CUA_DRIVER_APP")
	launchArgs := cuaDaemonLaunchArgs(appPath)
	logf("[cua] daemon absent, launching: open %s", strings.Join(launchArgs, " "))
	if out, err := exec.CommandContext(ctx, "open", launchArgs...).CombinedOutput(); err != nil {
		return fmt.Errorf("cua: 拉起 CuaDriver daemon 失败: %v (%s)；确认 CuaDriver.app 可用（内置 resources/cua 或 /Applications 安装；daemon 形态依赖 app 身份）",
			err, strings.TrimSpace(string(out)))
	}
	deadline := time.Now().Add(cuaDaemonReadyTimeout)
	for time.Now().Before(deadline) {
		if cuaDaemonAlive(sock) {
			logf("[cua] daemon ready: %s", sock)
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("cua: 等待 CuaDriver daemon 就绪被取消: %w", ctx.Err())
		case <-time.After(cuaDaemonPollInterval):
		}
	}
	return fmt.Errorf("cua: CuaDriver daemon 拉起后 %.0fs 内未监听 %s；运行 `cua-driver doctor` 诊断，或 `cua-driver permissions grant` 授予辅助功能/屏幕录制权限后重试",
		cuaDaemonReadyTimeout.Seconds(), sock)
}

// findCuaDriver 探测 cua-driver 二进制：CUA_DRIVER_PATH 环境变量 →
// CUA_DRIVER_APP（内置 app 派生 Contents/MacOS/cua-driver）→ PATH → 常见安装
// 路径。找不到返回空串，由 cua.status 报告 unavailable。
func findCuaDriver() string {
	if p := os.Getenv("CUA_DRIVER_PATH"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	// CUA_DRIVER_APP：桌面端内置 CuaDriver.app 路径（仅设 APP 未设 PATH 时兜底）
	if app := os.Getenv("CUA_DRIVER_APP"); app != "" {
		p := filepath.Join(app, "Contents", "MacOS", "cua-driver")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("cua-driver"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, ".local", "bin", "cua-driver"),
		"/Applications/CuaDriver.app/Contents/MacOS/cua-driver",
		"/usr/local/bin/cua-driver",
		"/opt/homebrew/bin/cua-driver",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// ---- MCP mini-client（stdio 换行 JSON-RPC：initialize / tools/call） ----

// mcpContent 是 tools/call 应答的 content 项（只关心 text/image 两类）。
type mcpContent struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Data     string `json:"data"` // image: base64
	MimeType string `json:"mimeType"`
}

// mcpResult 是 tools/call 的 result 体。
type mcpResult struct {
	Content           []mcpContent   `json:"content"`
	StructuredContent map[string]any `json:"structuredContent"`
	IsError           bool           `json:"isError"`
}

// mcpResponse 是 JSON-RPC 应答信封。
type mcpResponse struct {
	ID     int              `json:"id"`
	Result *json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// cuaMcp 持有 cua-driver mcp 持久子进程。
//
// 锁分层：
//   - callMu：串行全部 tools/call（stateful 语义：snapshot→action 的 token
//     代次；与服务端 procs SerialPerTarget 同向）；
//   - mu：只保护连接状态字段（cmd/stdin/pending/alive/nextID）的短临界区，
//     应答等待不持锁（readLoop 投递需要 mu，持锁等待会死锁）。
type cuaMcp struct {
	bin               string
	logf              func(string, ...any)
	callMu            sync.Mutex
	writeGate         chan struct{}
	mu                sync.Mutex
	cmd               *exec.Cmd
	stdin             io.WriteCloser
	pending           map[int]chan mcpResponse
	nextID            int
	alive             bool
	generation        uint64
	needsSessionReset bool
}

func newCuaMcp(bin string, logf func(string, ...any)) *cuaMcp {
	return &cuaMcp{bin: bin, logf: logf, writeGate: make(chan struct{}, 1), pending: map[int]chan mcpResponse{}, nextID: 1}
}

// ensure 保证子进程存活且完成 MCP 握手（懒启动；死进程清理后重生）。
// 调用方须持 callMu。
func (m *cuaMcp) ensure(ctx context.Context) error {
	m.mu.Lock()
	if m.alive && m.cmd != nil {
		reset := m.needsSessionReset
		m.mu.Unlock()
		if reset {
			// Repair the transport's implicit lifecycle before a NEW command.
			// Reissuing the failed action here could duplicate its side effects.
			if _, err := m.callTool(ctx, "start_session", map[string]any{}); err != nil {
				return fmt.Errorf("cua-driver session recovery: %w", err)
			}
			m.mu.Lock()
			m.needsSessionReset = false
			m.mu.Unlock()
		}
		return nil
	}
	m.killLocked()
	m.mu.Unlock()

	sock := cuaDaemonSocketPath()
	if runtime.GOOS == "darwin" {
		if err := ensureCuaDaemon(ctx, m.logf); err != nil {
			return err
		}
	}
	args := cuaMcpArgs(runtime.GOOS, sock)
	cmd := exec.Command(m.bin, args...)
	m.logf("[cua] starting driver: %s %s", m.bin, strings.Join(args, " "))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("cua-driver stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("cua-driver stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("cua-driver stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("cua-driver start: %w", err)
	}

	m.mu.Lock()
	m.cmd = cmd
	m.generation++
	m.needsSessionReset = false
	m.stdin = stdin
	m.mu.Unlock()

	// stderr 只进日志（驱动的 tracing 输出）
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			m.logf("[cua] driver: %s", sc.Text())
		}
	}()
	// stdout 读循环：按行分发到 pending 表
	go m.readLoop(stdout)
	// 进程退出：标记死亡并拒绝全部在途调用
	go func() {
		werr := cmd.Wait()
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.cmd != cmd {
			return
		}
		m.alive = false
		for id, ch := range m.pending {
			ch <- mcpResponse{ID: id, Error: &struct {
				Message string `json:"message"`
			}{Message: fmt.Sprintf("cua-driver exited: %v", werr)}}
			delete(m.pending, id)
		}
		m.cmd = nil
		m.stdin = nil
		m.logf("[cua] driver exited: %v", werr)
	}()

	// 握手：initialize + notifications/initialized
	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := m.request(initCtx, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "aic-host", "version": "0"},
	}); err != nil {
		m.mu.Lock()
		m.killLocked()
		m.mu.Unlock()
		return fmt.Errorf("cua-driver mcp initialize: %w", err)
	}
	if err := m.notify(initCtx, "notifications/initialized", map[string]any{}); err != nil {
		return err
	}
	m.mu.Lock()
	if m.cmd != cmd {
		m.mu.Unlock()
		return fmt.Errorf("cua-driver closed during initialization")
	}
	m.alive = true
	m.mu.Unlock()
	m.logf("[cua] mcp initialized (pid %d)", cmd.Process.Pid)
	return nil
}

func (m *cuaMcp) readLoop(stdout io.ReadCloser) {
	reader := bufio.NewReaderSize(stdout, 4*1024*1024)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return // 进程退出由 Wait goroutine 收尾
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// 防线：单行最大 32MB（AX 树/内联截图），防内存放大
		if len(line) > 32*1024*1024 {
			m.logf("[cua] driver output line overflow (%d bytes)", len(line))
			continue
		}
		var resp mcpResponse
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			continue
		}
		m.mu.Lock()
		if ch, ok := m.pending[resp.ID]; ok {
			ch <- resp
			delete(m.pending, resp.ID)
		}
		m.mu.Unlock()
	}
}

// request 发送一个 JSON-RPC 请求并等应答（ctx 控超时；等待不持 mu）。
func (m *cuaMcp) request(ctx context.Context, method string, params any) (*json.RawMessage, error) {
	select {
	case m.writeGate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	m.mu.Lock()
	if m.cmd == nil || m.stdin == nil {
		m.mu.Unlock()
		<-m.writeGate
		return nil, fmt.Errorf("cua-driver not running")
	}
	id := m.nextID
	m.nextID++
	ch := make(chan mcpResponse, 1)
	m.pending[id] = ch
	cmd, stdin := m.cmd, m.stdin
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.pending, id); m.mu.Unlock() }()
	body, werr := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	})
	if werr == nil {
		werr = m.writeMessage(ctx, cmd, stdin, append(body, '\n'))
	}
	<-m.writeGate
	if werr != nil {
		return nil, fmt.Errorf("cua-driver write: %w", werr)
	}
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("%s", resp.Error.Message)
		}
		return resp.Result, nil
	case <-ctx.Done():
		m.mu.Lock()
		delete(m.pending, id)
		m.mu.Unlock()
		return nil, fmt.Errorf("cua-driver timeout: %s", method)
	}
}

func (m *cuaMcp) notify(ctx context.Context, method string, params any) error {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	if err != nil {
		return err
	}
	select {
	case m.writeGate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-m.writeGate }()
	m.mu.Lock()
	cmd, stdin := m.cmd, m.stdin
	m.mu.Unlock()
	if cmd == nil || stdin == nil {
		return fmt.Errorf("cua-driver not running")
	}
	return m.writeMessage(ctx, cmd, stdin, append(body, '\n'))
}

// Caller holds writeGate, never mu. Closing this generation's stdin interrupts
// a blocked write; a partial JSON-RPC line requires a fresh transport, not replay.
func (m *cuaMcp) writeMessage(ctx context.Context, cmd *exec.Cmd, stdin io.WriteCloser, body []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	finished := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		m.mu.Lock()
		if m.cmd == cmd {
			m.killLocked()
		}
		m.mu.Unlock()
		close(finished)
	})
	n, err := stdin.Write(body)
	if !stop() {
		<-finished
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	} else if err == nil && n != len(body) {
		err = io.ErrShortWrite
	}
	if err != nil {
		m.mu.Lock()
		if m.cmd == cmd {
			m.killLocked()
		}
		m.mu.Unlock()
	}
	return err
}

// call never replays an interaction after a transport/session error.
func (m *cuaMcp) call(ctx context.Context, name string, args map[string]any) (*mcpResult, error) {
	m.callMu.Lock()
	defer m.callMu.Unlock()
	if err := m.ensure(ctx); err != nil {
		return nil, err
	}
	return m.callTool(ctx, name, args)
}

// callEpoch refuses to revive a runtime underneath an existing UI target.
func (m *cuaMcp) callEpoch(ctx context.Context, epoch uint64, name string, args map[string]any) (*mcpResult, error) {
	m.callMu.Lock()
	defer m.callMu.Unlock()
	m.mu.Lock()
	valid := m.alive && m.generation == epoch
	m.mu.Unlock()
	if !valid {
		return nil, fmt.Errorf("driver runtime changed; rediscover targets")
	}
	return m.callTool(ctx, name, args)
}

// callTool 单次 tools/call（调用方须持 callMu；不做会话修复）。
func (m *cuaMcp) callTool(ctx context.Context, name string, args map[string]any) (result *mcpResult, callErr error) {
	defer func() {
		if cuaSessionEndedErr(callErr) {
			m.mu.Lock()
			defer m.mu.Unlock()
			if !m.needsSessionReset {
				m.needsSessionReset = true
				m.generation++
			}
		}
	}()
	raw, err := m.request(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return nil, err
	}
	var res mcpResult
	if raw != nil {
		if err := json.Unmarshal(*raw, &res); err != nil {
			return nil, fmt.Errorf("cua-driver invalid result: %w", err)
		}
	}
	if res.IsError {
		var texts []string
		for _, c := range res.Content {
			if c.Text != "" {
				texts = append(texts, c.Text)
			}
		}
		if len(texts) == 0 {
			return nil, fmt.Errorf("tool %s failed", name)
		}
		return nil, fmt.Errorf("%s", strings.Join(texts, "\n"))
	}
	return &res, nil
}

// cuaSessionEndedErr 判定驱动侧会话结束；上层废弃绑定，不重放当前动作。
func cuaSessionEndedErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "has ended") && strings.Contains(msg, "start_session")
}

// killLocked 终止子进程并清理状态。调用方须持 mu。
func (m *cuaMcp) killLocked() {
	if m.stdin != nil {
		_ = m.stdin.Close()
	}
	if m.cmd != nil && m.cmd.Process != nil {
		m.cmd.Process.Kill()
	}
	m.cmd = nil
	m.stdin = nil
	m.alive = false
	for id, ch := range m.pending {
		ch <- mcpResponse{ID: id, Error: &struct {
			Message string `json:"message"`
		}{Message: "cua-driver killed"}}
		delete(m.pending, id)
	}
}

// AvailablePath reports the native executable without starting a process.
func AvailablePath() string { return findCuaDriver() }
