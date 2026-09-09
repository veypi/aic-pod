package host

// cua 一级命令（§5.10）：本机 GUI 自动化，Go 原生桥接 cua-driver。
//
// 链路：exec cua 请求 → runCua（本文件）→ MCP tools/call（stdio 换行
// JSON-RPC）→ `cua-driver mcp` 持久子进程。无 JS/壳通道中间层。
//
//   - 探测模型（§5.1）：buildCommandTable 启动探测 cua-driver 二进制
//     （CUA_DRIVER_PATH → PATH → 常见安装路径），探测到才声明；
//   - 子进程生命周期：首次 cua 调用懒启动，常驻复用（snapshot 的
//     element_token 缓存/光标/录制状态随 MCP 连接连续）；死亡则当次调用
//     报错暴露、下次调用自动重生一次；进程级单例随 host 进程回收；
//   - 连接形态（darwin）：daemon 唯一形态——`mcp --socket <默认 socket>` 代理到
//     CuaDriver.app 常驻 daemon（AppKit 主线程宿主身份，虚拟光标浮层/录制
//     回放/前台投递依赖它；TCC 授权归 CuaDriver.app，用户授权一次）。daemon
//     缺席时自动拉起（open -n -g -a CuaDriver --args serve）并轮询 socket；
//     拉起失败/未授权明确报错，永不静默换身份。非 darwin 暂无 daemon 自动
//     拉起环境，保留 `mcp --direct` 过渡；
//   - 沙箱：cua-driver 本质是控制本机 GUI 的宿主体外能力（辅助功能/录屏
//     授权），不进 exec_procs 沙箱（与 provider 壳通道同语义边界）；
//   - 串行：stateful 语义——进程内互斥锁串行全部 cua 调用（snapshot→action
//     的 token 代次），与服务端 procs SerialPerTarget 同向；
//   - 产物：snapshot 正文（tree_markdown + elements JSON 全量）落会话工作区
//     .cua/（sessionWorkDir）——snap-<ts>.txt；返回精简（摘要 + depth<=3 顶层
//     元素清单 + 正文路径），深度细节进 txt 需 fs 读取；--png：另出窗口截图
//     snap-<ts>.png 落盘并以 image_data 附返回（服务端直接投喂模型视觉，无需
//     再 fs.read），默认不带（省 token），需要看图时显式请求。
//     doctor 子命令走一次性 CLI（自检含安装/权限/守护进程诊断，不走 MCP）。

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/aic-pod/libs/vcore"
)

// cuaMcpArgs 返回 mcp 子进程参数：darwin = daemon 唯一形态（socket 代理，
// daemon 拉起/就绪由 ensureCuaDaemon 保证）；非 darwin 无 CuaDriver.app
// daemon 自动拉起环境，保留 --direct 过渡。
func cuaMcpArgs(goos, sock string) []string {
	if goos == "darwin" {
		return []string{"mcp", "--socket", sock}
	}
	return []string{"mcp", "--direct", "--grant", "existing-profile"}
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
// --grant existing-profile 解锁“绑定用户真实浏览器”能力（驱动 standard 模式
// 要求 trusted launch grant；平台层 bprepare existing 仍逐次 Danger 审批）。
func cuaDaemonLaunchArgs() []string {
	return []string{"-n", "-g", "-a", "CuaDriver", "--args", "serve", "--grant", "existing-profile"}
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

	logf("[cua] daemon absent, launching: open %s", strings.Join(cuaDaemonLaunchArgs(), " "))
	if out, err := exec.CommandContext(ctx, "open", cuaDaemonLaunchArgs()...).CombinedOutput(); err != nil {
		return fmt.Errorf("cua: 拉起 CuaDriver daemon 失败: %v (%s)；确认 /Applications/CuaDriver.app 已安装（daemon 形态依赖 app 身份）",
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

// findCuaDriver 探测 cua-driver 二进制：CUA_DRIVER_PATH 环境变量 → PATH →
// 常见安装路径。找不到返回空串（buildCommandTable 则不声明 cua）。
func findCuaDriver() string {
	if p := os.Getenv("CUA_DRIVER_PATH"); p != "" {
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
	Type string `json:"type"`
	Text string `json:"text"`
	Data string `json:"data"` // image: base64
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
	bin     string
	logf    func(string, ...any)
	callMu  sync.Mutex
	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.Writer
	pending map[int]chan mcpResponse
	nextID  int
	alive   bool
	// 浏览器绑定（typed browser 家族：bprepare/browser-state 提取，
	// navigate/bclick/btype 注入 target/tab，browser-state 无 --pid 时注入 pid）
	browserTargetID string
	browserTabID    string
	browserPid      int
}

func newCuaMcp(bin string, logf func(string, ...any)) *cuaMcp {
	return &cuaMcp{bin: bin, logf: logf, pending: map[int]chan mcpResponse{}, nextID: 1}
}

// ensure 保证子进程存活且完成 MCP 握手（懒启动；死进程清理后重生）。
// 调用方须持 callMu。
func (m *cuaMcp) ensure(ctx context.Context) error {
	m.mu.Lock()
	if m.alive && m.cmd != nil {
		m.mu.Unlock()
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
	m.notify("notifications/initialized", map[string]any{})
	m.mu.Lock()
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
	m.mu.Lock()
	if m.cmd == nil || m.stdin == nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("cua-driver not running")
	}
	id := m.nextID
	m.nextID++
	ch := make(chan mcpResponse, 1)
	m.pending[id] = ch
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	})
	_, werr := m.stdin.Write(append(body, '\n'))
	m.mu.Unlock()
	if werr != nil {
		m.mu.Lock()
		delete(m.pending, id)
		m.mu.Unlock()
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

func (m *cuaMcp) notify(method string, params any) {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stdin != nil {
		m.stdin.Write(append(body, '\n'))
	}
}

// call 执行 tools/call（callMu 串行 + 懒启动）；isError 转为 error。
func (m *cuaMcp) call(ctx context.Context, name string, args map[string]any) (*mcpResult, error) {
	m.callMu.Lock()
	defer m.callMu.Unlock()
	if err := m.ensure(ctx); err != nil {
		return nil, err
	}
	callCtx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, 120*time.Second)
		defer cancel()
	}
	raw, err := m.request(callCtx, "tools/call", map[string]any{"name": name, "arguments": args})
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

// killLocked 终止子进程并清理状态。调用方须持 mu。
func (m *cuaMcp) killLocked() {
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

// ---- 进程级 cua 运行时单例（探测到二进制的 host 才有） ----

var cuaRt *cuaMcp

// initCuaRuntime 在 buildCommandTable 探测到 cua-driver 时建立运行时单例。
func initCuaRuntime(logf func(string, ...any)) {
	if cuaRt == nil {
		if bin := findCuaDriver(); bin != "" {
			cuaRt = newCuaMcp(bin, logf)
		}
	}
}

// ---- argv → MCP tool 参数装配 ----

// cuaValueFlags 带值 flag 表（与 vcore levels.go 同源，禁止漂移）。
var cuaValueFlags = map[string]bool{
	"--pid": true, "--window": true, "--token": true,
	"--x": true, "--y": true, "--x1": true, "--y1": true, "--x2": true, "--y2": true,
	"--text": true, "--app": true, "--value": true, "--path": true,
	"--direction": true, "--amount": true, "--width": true, "--height": true,
	"--delivery": true, "--button": true,
	"--url": true, "--query": true, "--ref": true, "--mode": true, "--route": true,
	"--grep": true, "--context": true, "--target": true, "--tab": true,
	"--code": true, "--file": true,
}

var cuaBoolFlags = map[string]bool{"--png": true, "--replace": true, "--isolated": true}

var errCuaFlagAbsent = fmt.Errorf("flag absent")

type cuaCall struct {
	cli           bool           // doctor：一次性 CLI
	tool          string         // MCP 工具名
	args          map[string]any // tools/call 参数
	snapshot      bool           // snapshot：图文成对落盘 + 返回精简（runCua 特化）
	png           bool           // snapshot --png：出图落盘 + 以 image_data 附返回（模型直接看图）
	grep          string         // snapshot --grep：树文本类 grep 过滤关键词
	grepCtx       int            // --context：命中行前后保留行数（默认 2）
	browserBind   bool           // browser-state/bprepare：应答里提取绑定
	browserTarget bool           // navigate/bclick/btype：调用前注入浏览器绑定
	browserSess   bool           // bend：仅注入 session
	script        bool           // run：JS 脚本执行（runCuaScript 特化）
	code          string         // run --code：内联脚本全文
	file          string         // run --file：host 绝对路径脚本文件
	paste         string         // type：非 ASCII 文本改走剪贴板粘贴（clipboard_write + 粘贴热键）
}

// mapCuaArgv 把 cua argv 映射为 MCP 调用。未知子命令/非法参数报错。
func mapCuaArgv(argv []string) (*cuaCall, error) {
	flags := map[string]string{}
	flagsAll := map[string][]string{} // 可重复 flag（如 --url）全量收集
	bools := map[string]bool{}
	var positional []string
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if cuaValueFlags[a] {
			if i+1 >= len(argv) {
				return nil, fmt.Errorf("flag %s requires a value", a)
			}
			i++
			flags[a] = argv[i]
			flagsAll[a] = append(flagsAll[a], argv[i])
		} else if cuaBoolFlags[a] {
			bools[a] = true
		} else if strings.HasPrefix(a, "--") {
			return nil, fmt.Errorf("unknown flag %s", a)
		} else {
			positional = append(positional, a)
		}
	}
	sub := ""
	if len(positional) > 0 {
		sub = positional[0]
	}

	// num 取带值 flag 的数字；缺省返回 errCuaFlagAbsent（required 语义由调用方定）。
	num := func(name string) (float64, error) {
		v, ok := flags[name]
		if !ok {
			return 0, errCuaFlagAbsent
		}
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, fmt.Errorf("flag %s must be a number, got %s", name, v)
		}
		return n, nil
	}
	// optInt/optNum：可选数字落 args[key]（缺省跳过）。
	optInt := func(args map[string]any, name, key string) error {
		n, err := num(name)
		if err == errCuaFlagAbsent {
			return nil
		}
		if err != nil {
			return err
		}
		args[key] = int(n)
		return nil
	}
	optNum := func(args map[string]any, name, key string) error {
		n, err := num(name)
		if err == errCuaFlagAbsent {
			return nil
		}
		if err != nil {
			return err
		}
		args[key] = n
		return nil
	}
	// reqInt/reqNum：必填数字（缺省报错）。
	reqInt := func(args map[string]any, name, key string) error {
		n, err := num(name)
		if err == errCuaFlagAbsent {
			return fmt.Errorf("flag %s is required", name)
		}
		if err != nil {
			return err
		}
		args[key] = int(n)
		return nil
	}
	reqNum := func(args map[string]any, name, key string) error {
		n, err := num(name)
		if err == errCuaFlagAbsent {
			return fmt.Errorf("flag %s is required", name)
		}
		if err != nil {
			return err
		}
		args[key] = n
		return nil
	}
	// targetArgs 动作类工具的公共目标参数（pid/window/token/xy/delivery）。
	targetArgs := func() (map[string]any, error) {
		args := map[string]any{}
		if err := optInt(args, "--pid", "pid"); err != nil {
			return nil, err
		}
		if err := optInt(args, "--window", "window_id"); err != nil {
			return nil, err
		}
		if v, ok := flags["--token"]; ok {
			args["element_token"] = v
		}
		if err := optNum(args, "--x", "x"); err != nil {
			return nil, err
		}
		if err := optNum(args, "--y", "y"); err != nil {
			return nil, err
		}
		if v, ok := flags["--delivery"]; ok {
			args["delivery_mode"] = v
		}
		return args, nil
	}

	switch sub {
	case "run":
		// JS 脚本执行（cua_run.go）：--code 内联 / --file host 绝对路径，二选一必填。
		// 恒 Danger(3) 逐次审批（levels.go），脚本全文随审批可见。
		code := flags["--code"]
		file := flags["--file"]
		if file == "" && len(positional) > 1 {
			file = positional[1]
		}
		if code != "" && file != "" {
			return nil, fmt.Errorf("run: --code 与 --file 二选一")
		}
		if code == "" && file == "" {
			return nil, fmt.Errorf("run requires --code <js> or --file <path>")
		}
		return &cuaCall{script: true, code: code, file: file}, nil
	case "doctor":
		return &cuaCall{cli: true}, nil
	case "apps":
		return &cuaCall{tool: "list_apps", args: map[string]any{}}, nil
	case "windows":
		args := map[string]any{}
		if err := optInt(args, "--pid", "pid"); err != nil {
			return nil, err
		}
		return &cuaCall{tool: "list_windows", args: args}, nil
	case "snapshot":
		args := map[string]any{}
		if err := reqInt(args, "--pid", "pid"); err != nil {
			return nil, err
		}
		if err := reqInt(args, "--window", "window_id"); err != nil {
			return nil, err
		}
		c := &cuaCall{tool: "get_window_state", args: args, snapshot: true}
		// --png：出图（include_screenshot + screenshot_out_file 由 runCua 填）——
		// 截图落盘 .cua/snap-<ts>.png 并以 image_data（data URI）附在返回 attrs，
		// 服务端统一落盘投递为模型视觉输入；默认不出图（省 token），需要看图时
		// 显式请求。
		if bools["--png"] {
			c.png = true
		}
		// --grep/--context：树文本类 grep 过滤（runCua 本地过滤，不经驱动）
		if kw, ok := flags["--grep"]; ok && kw != "" {
			c.grep = kw
			c.grepCtx = 2
			if v, ok2 := flags["--context"]; ok2 {
				if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 20 {
					c.grepCtx = n
				}
			}
		}
		return c, nil
	case "launch":
		name := flags["--app"]
		if name == "" && len(positional) > 1 {
			name = positional[1]
		}
		if name == "" {
			return nil, fmt.Errorf("launch requires --app <name>")
		}
		args := map[string]any{"name": name}
		if urls := flagsAll["--url"]; len(urls) > 0 {
			args["urls"] = urls
		}
		return &cuaCall{tool: "launch_app", args: args}, nil
	case "browser-state":
		// typed browser 绑定（BROWSER.md 正解）：pid+window 绑定窗口，
		// 应答 structuredContent 提取 target_id/tab_id 存绑定供 navigate 等注入
		args := map[string]any{"snapshot_format": "semantic_v2"}
		if err := optInt(args, "--pid", "pid"); err != nil {
			return nil, err
		}
		if err := optInt(args, "--window", "window_id"); err != nil {
			return nil, err
		}
		if v, ok := flags["--target"]; ok {
			args["target_id"] = v
		}
		if v, ok := flags["--tab"]; ok {
			args["tab_id"] = v
		}
		if v, ok := flags["--query"]; ok {
			args["query"] = v
		}
		return &cuaCall{tool: "get_browser_state", args: args, browserBind: true}, nil
	case "navigate":
		url := flags["--url"]
		if url == "" && len(positional) > 1 {
			url = positional[1]
		}
		if url == "" {
			return nil, fmt.Errorf("navigate requires --url <url>")
		}
		return &cuaCall{tool: "browser_navigate", args: map[string]any{"url": url}, browserTarget: true}, nil
	case "bprepare":
		// 绑定用户真实浏览器（existing_profile）：驱动级需 --grant existing-profile
		// （spawn 已带），平台层 Danger 逐次审批——会开启用户浏览器的远程调试。
		// --isolated：驱动自持隔离 profile（标准模式例行，不需登录态的任务优先）。
		if bools["--isolated"] {
			return &cuaCall{tool: "browser_prepare", browserBind: true, args: map[string]any{
				"profile": map[string]any{"mode": "isolated_new"}, "allow_launch": true}}, nil
		}
		args := map[string]any{"strategy": map[string]any{"kind": "existing_profile"}}
		if err := reqInt(args, "--pid", "pid"); err != nil {
			return nil, err
		}
		if err := reqInt(args, "--window", "window_id"); err != nil {
			return nil, err
		}
		return &cuaCall{tool: "browser_prepare", args: args, browserBind: true}, nil
	case "bend":
		return &cuaCall{tool: "end_session", args: map[string]any{}, browserSess: true}, nil
	case "bclick":
		args := map[string]any{}
		if v, ok := flags["--ref"]; ok {
			args["ref"] = v
		}
		if err := optNum(args, "--x", "x"); err != nil {
			return nil, err
		}
		if err := optNum(args, "--y", "y"); err != nil {
			return nil, err
		}
		if v, ok := flags["--route"]; ok {
			args["input_route"] = v
		}
		if args["ref"] == nil && args["x"] == nil {
			return nil, fmt.Errorf("bclick requires --ref <ref> or --x/--y")
		}
		return &cuaCall{tool: "browser_click", args: args, browserTarget: true}, nil
	case "btype":
		ref := flags["--ref"]
		if ref == "" {
			return nil, fmt.Errorf("btype requires --ref <ref>")
		}
		text, ok := flags["--text"]
		if !ok && len(positional) > 1 {
			text = positional[1]
			ok = true
		}
		if !ok {
			return nil, fmt.Errorf("btype requires --text <text>")
		}
		args := map[string]any{"ref": ref, "text": text}
		if v, ok2 := flags["--mode"]; ok2 {
			args["mode"] = v
		}
		if bools["--replace"] {
			args["replace"] = true
		}
		return &cuaCall{tool: "browser_type", args: args, browserTarget: true}, nil
	case "click", "dclick", "rclick":
		tool := map[string]string{"click": "click", "dclick": "double_click", "rclick": "right_click"}[sub]
		args, err := targetArgs()
		if err != nil {
			return nil, err
		}
		if sub == "click" {
			if v, ok := flags["--button"]; ok {
				args["button"] = v
			}
		}
		return &cuaCall{tool: tool, args: args}, nil
	case "type":
		text, ok := flags["--text"]
		if !ok && len(positional) > 1 {
			text = positional[1]
			ok = true
		}
		if !ok {
			return nil, fmt.Errorf("type requires --text <text>")
		}
		args, err := targetArgs()
		if err != nil {
			return nil, err
		}
		// 非 ASCII（中文等）不走逐键合成：IME 会把字母转候选/吞掉。改走
		// 剪贴板粘贴（runCua 的 paste 分支），平台无关。
		if hasNonASCII(text) {
			return &cuaCall{tool: "type_text", args: args, paste: text}, nil
		}
		args["text"] = text
		return &cuaCall{tool: "type_text", args: args}, nil
	case "key":
		if len(positional) < 2 {
			return nil, fmt.Errorf("key requires a key name (enter/tab/esc/...)")
		}
		args, err := targetArgs()
		if err != nil {
			return nil, err
		}
		args["key"] = positional[1]
		return &cuaCall{tool: "press_key", args: args}, nil
	case "hotkey":
		if len(positional) < 2 {
			return nil, fmt.Errorf("hotkey requires a combo (e.g. cmd+c)")
		}
		args, err := targetArgs()
		if err != nil {
			return nil, err
		}
		// 实现走 press_key+modifiers 而非 hotkey 工具：hotkey 对 Blender 等原生
		// OpenGL app 后台投递失败（驱动返 delivery_failed）且修饰键会丟失
		// （cmd+a 变裸 'a'）；press_key 的 modifiers 数组是实测可靠路径
		// （2026-09-09 Blender 记录：super+a / ctrl+shift+s 均生效）。
		parts := strings.Split(positional[1], "+")
		if len(parts) < 2 {
			return nil, fmt.Errorf("hotkey requires a combo (e.g. cmd+c)")
		}
		args["key"] = parts[len(parts)-1]
		args["modifiers"] = parts[:len(parts)-1]
		return &cuaCall{tool: "press_key", args: args}, nil
	case "scroll":
		direction := flags["--direction"]
		if direction == "" && len(positional) > 1 {
			direction = positional[1]
		}
		if direction == "" {
			return nil, fmt.Errorf("scroll requires --direction up|down|left|right")
		}
		args, err := targetArgs()
		if err != nil {
			return nil, err
		}
		args["direction"] = direction
		if err := optInt(args, "--amount", "amount"); err != nil {
			return nil, err
		}
		return &cuaCall{tool: "scroll", args: args}, nil
	case "drag":
		args, err := targetArgs()
		if err != nil {
			return nil, err
		}
		for _, pair := range [][2]string{{"--x1", "from_x"}, {"--y1", "from_y"}, {"--x2", "to_x"}, {"--y2", "to_y"}} {
			if err := reqNum(args, pair[0], pair[1]); err != nil {
				return nil, err
			}
		}
		return &cuaCall{tool: "drag", args: args}, nil
	case "move":
		args := map[string]any{}
		if err := reqNum(args, "--x", "x"); err != nil {
			return nil, err
		}
		if err := reqNum(args, "--y", "y"); err != nil {
			return nil, err
		}
		return &cuaCall{tool: "move_cursor", args: args}, nil
	case "front":
		// 前台激活应用（bring_to_front）：焦点代理面/前台投递（IME 敏感文本
		// 输入等）的前提。driver 明确会窃取前台焦点——Danger(3)（levels.go）。
		args := map[string]any{}
		if err := reqInt(args, "--pid", "pid"); err != nil {
			return nil, err
		}
		if err := optInt(args, "--window", "window_id"); err != nil {
			return nil, err
		}
		return &cuaCall{tool: "bring_to_front", args: args}, nil
	case "set-value":
		tok := flags["--token"]
		if tok == "" {
			return nil, fmt.Errorf("set-value requires --token <element_token>")
		}
		value, ok := flags["--value"]
		if !ok && len(positional) > 1 {
			value = positional[1]
			ok = true
		}
		if !ok {
			return nil, fmt.Errorf("set-value requires --value <value>")
		}
		args := map[string]any{"element_token": tok, "value": value}
		if err := optInt(args, "--pid", "pid"); err != nil {
			return nil, err
		}
		return &cuaCall{tool: "set_value", args: args}, nil
	case "menu":
		pathStr := flags["--path"]
		if pathStr == "" && len(positional) > 1 {
			pathStr = positional[1]
		}
		if pathStr == "" {
			return nil, fmt.Errorf(`menu requires --path "File>Save"`)
		}
		args := map[string]any{"path": strings.Split(pathStr, ">")}
		if err := reqInt(args, "--pid", "pid"); err != nil {
			return nil, err
		}
		if err := optInt(args, "--window", "window_id"); err != nil {
			return nil, err
		}
		return &cuaCall{tool: "invoke_menu", args: args}, nil
	case "set-frame":
		args := map[string]any{}
		if err := reqInt(args, "--pid", "pid"); err != nil {
			return nil, err
		}
		if err := reqInt(args, "--window", "window_id"); err != nil {
			return nil, err
		}
		for _, pair := range [][2]string{{"--x", "x"}, {"--y", "y"}, {"--width", "width"}, {"--height", "height"}} {
			if err := reqNum(args, pair[0], pair[1]); err != nil {
				return nil, err
			}
		}
		return &cuaCall{tool: "set_window_frame", args: args}, nil
	case "clipboard":
		if len(positional) < 2 {
			return nil, fmt.Errorf("clipboard requires read|write")
		}
		switch positional[1] {
		case "read":
			// include_text：驱动默认省略文本，平台语义 = 读出文本
			return &cuaCall{tool: "clipboard_read", args: map[string]any{"include_text": true}}, nil
		case "write":
			text, ok := flags["--text"]
			if !ok && len(positional) > 2 {
				text = positional[2]
				ok = true
			}
			if !ok {
				return nil, fmt.Errorf("clipboard write requires a text argument")
			}
			return &cuaCall{tool: "clipboard_write", args: map[string]any{"text": text}}, nil
		}
		return nil, fmt.Errorf("clipboard requires read|write")
	}
	return nil, fmt.Errorf("unknown cua subcommand %q", sub)
}

// setBrowserBinding/browserBinding 存取 typed browser 绑定（调用方持 callMu，
// 但绑定读写也走 mu，防未来并发路径）。
func (m *cuaMcp) setBrowserBinding(targetID, tabID string) {
	m.mu.Lock()
	m.browserTargetID = targetID
	m.browserTabID = tabID
	m.mu.Unlock()
}

func (m *cuaMcp) browserBinding() (string, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.browserTargetID, m.browserTabID
}

func (m *cuaMcp) setBrowserPid(pid int) {
	m.mu.Lock()
	m.browserPid = pid
	m.mu.Unlock()
}

func (m *cuaMcp) browserPidValue() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.browserPid
}

// cuaFindStrKeys 在嵌套 map/切片里找指定键的首个非空字符串值（限深 4 层，
// 用于从 get_browser_state 应答提取 target_id/tab_id——该工具无 outputSchema，
// 防御式扫描）。
func cuaFindStrKeys(v any, keys map[string]bool, out map[string]string, depth int) {
	if depth > 4 {
		return
	}
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if keys[k] {
				if s, ok := x.(string); ok && s != "" && out[k] == "" {
					out[k] = s
				}
			} else {
				cuaFindStrKeys(x, keys, out, depth+1)
			}
		}
	case []any:
		for _, x := range t {
			cuaFindStrKeys(x, keys, out, depth+1)
		}
	}
}

// cuaFirstWindowID 从 list_windows 应答挑第一个可见窗口（无可见则第一个）。
func cuaFirstWindowID(sc map[string]any) int {
	wins, _ := sc["windows"].([]any)
	first := 0
	for _, w := range wins {
		wm, _ := w.(map[string]any)
		idF, _ := wm["window_id"].(float64)
		id := int(idF)
		if id == 0 {
			continue
		}
		if first == 0 {
			first = id
		}
		if on, _ := wm["is_on_screen"].(bool); on {
			return id
		}
	}
	return first
}

// grepTree 对 AX 树文本做类 grep 过滤：命中行（大小写不敏感）+ 祖先缩进链
// + 前后 ctxLines 行，间隙插 "…"。返回过滤文本与命中行里的元素索引（[N]，
// 供 token 图例）。无命中返回空串。
func grepTree(text, kw string, ctxLines int) (string, []int) {
	lines := strings.Split(text, "\n")
	ind := make([]int, len(lines))
	for i, ln := range lines {
		t := strings.TrimLeft(ln, " ")
		if strings.HasPrefix(t, "- ") {
			ind[i] = len(ln) - len(t)
		} else {
			ind[i] = -1 // 非树行（头部/空行）：视作根，总是保留
		}
	}
	kwLow := strings.ToLower(kw)
	keep := make([]bool, len(lines))
	var elemIdx []int
	for i, ln := range lines {
		if !strings.Contains(strings.ToLower(ln), kwLow) {
			continue
		}
		// 命中行的首个 [N] 元素索引
		if t := strings.TrimLeft(ln, " "); strings.HasPrefix(t, "- [") {
			rest := t[3:]
			if end := strings.Index(rest, "]"); end > 0 {
				if n, err := strconv.Atoi(rest[:end]); err == nil {
					elemIdx = append(elemIdx, n)
				}
			}
		}
		// 前后 ctxLines 行
		for j := i - ctxLines; j <= i+ctxLines; j++ {
			if j >= 0 && j < len(lines) {
				keep[j] = true
			}
		}
		// 祖先缩进链（逐级向上找更小缩进的行；非树行 ind=-1 保留并继续）
		th := ind[i]
		for j := i - 1; j >= 0; j-- {
			if ind[j] < 0 {
				keep[j] = true
				continue
			}
			if ind[j] < th {
				keep[j] = true
				th = ind[j]
			}
		}
	}
	if len(elemIdx) == 0 {
		return "", nil
	}
	kept := 0
	for _, k := range keep {
		if k {
			kept++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[grep %q: %d 命中，树 %d 行保留 %d 行，… 为省略段]\n", kw, len(elemIdx), len(lines), kept)
	prev := -2
	for i, ln := range lines {
		if !keep[i] {
			continue
		}
		if prev >= 0 && i > prev+1 {
			b.WriteString("…\n")
		}
		b.WriteString(ln)
		b.WriteString("\n")
		prev = i
	}
	// 尾部有省略也补 …（提示树下还有更多内容）
	if prev >= 0 && prev < len(lines)-1 {
		b.WriteString("…\n")
	}
	return b.String(), elemIdx
}

// runCua 执行 cua 命令（dispatch 特化分支，声明表命中后路由到此）。
func (c *Client) runCua(ctx context.Context, sid string, req *proto.ToolRequest, argv []string) (resp *proto.ToolResponse) {
	if cuaRt == nil {
		return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateError,
			Error: "cua: cua-driver not available on this host"}
	}
	mapped, err := mapCuaArgv(argv)
	if err != nil {
		return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateError, Error: "cua: " + err.Error()}
	}
	// IME 护栏（cua_ime.go）：键盘类动作前确保英文输入法——中文 IME 会把
	// Shift+A 这类组合键当输入法切换吃掉。发生切换/失败时在响应末尾附一行
	// 说明；已是英文则零开销静默。
	if (mapped.tool == "press_key" || mapped.tool == "type_text") && mapped.paste == "" {
		if note := imeGuard(); note != "" {
			defer func() {
				if resp != nil && resp.State == proto.StateCompleted {
					resp.Content = strings.TrimRight(resp.Content, "\n") + "\n[ime] " + note
				}
			}()
		}
	}
	// 非 ASCII 文本：剪贴板粘贴特化（逐键合成会被 IME 吞/转候选）
	if mapped.paste != "" {
		return c.runCuaPaste(ctx, req, mapped)
	}
	// run：JS 脚本执行（本地桥 + node runner，cua_run.go）
	if mapped.script {
		return c.runCuaScript(ctx, sid, req, mapped)
	}
	// doctor：一次性 CLI，不走 MCP
	if mapped.cli {
		cliCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		out, err := exec.CommandContext(cliCtx, cuaRt.bin, "doctor").CombinedOutput()
		resp := &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateCompleted, Content: string(out)}
		if err != nil {
			resp.State = proto.StateError
			resp.Error = fmt.Sprintf("doctor: %v", err)
		}
		return resp
	}
	// snapshot：图文成对落盘（截图 <base>.png + 正文 <base>.txt），父目录由本侧
	// 预建（驱动不创建目录）；正文落盘 + 返回精简见 cuaSnapshotResp。
	workDir := sessionWorkDir(sid)
	// typed browser 家族统一携带显式 session（驱动拒绝 implicit session 的浏览器操作）
	if mapped.browserBind || mapped.browserTarget || mapped.browserSess {
		if _, has := mapped.args["session"]; !has {
			mapped.args["session"] = "aic-" + sid
		}
	}
	// browser-state：无 --pid 时注入 bprepare 存的浏览器 pid；无 --window 时
	// 查该 pid 第一个可见窗口注入（隔离 profile 全流程免手填）
	if mapped.browserBind {
		if _, has := mapped.args["pid"]; !has {
			if pid := cuaRt.browserPidValue(); pid != 0 {
				mapped.args["pid"] = pid
			}
		}
		if _, has := mapped.args["window_id"]; !has {
			if pid, ok := mapped.args["pid"].(int); ok && pid != 0 {
				if wres, werr := cuaRt.call(ctx, "list_windows", map[string]any{"pid": pid}); werr == nil {
					if wid := cuaFirstWindowID(wres.StructuredContent); wid != 0 {
						mapped.args["window_id"] = wid
					}
				}
			}
		}
	}
	// typed browser 家族：navigate/bclick/btype 注入 browser-state 存的绑定
	if mapped.browserTarget {
		tid, tab := cuaRt.browserBinding()
		if tid == "" || tab == "" {
			return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateError,
				Error: "cua: 无浏览器绑定：先 browser-state [--pid N --window W] 绑定窗口"}
		}
		mapped.args["target_id"] = tid
		mapped.args["tab_id"] = tab
	}
	// snapshot：正文（AX 树全量）落盘 .cua/snap-<ts>.txt + 返回精简（grep 命中/
	// 顶层元素在返回里，全量在 txt）；--png 时另出截图（include_screenshot +
	// screenshot_out_file 见下）并以 image_data 附返回。
	if mapped.snapshot {
		cuaDir := filepath.Join(workDir, ".cua")
		if err := os.MkdirAll(cuaDir, 0o700); err != nil {
			return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateError, Error: "cua: " + err.Error()}
		}
		base := filepath.Join(cuaDir, fmt.Sprintf("snap-%d", time.Now().UnixMilli()))
		shotPath := base + ".png"
		txtPath := base + ".txt"
		if mapped.png {
			mapped.args["include_screenshot"] = true
			mapped.args["screenshot_out_file"] = shotPath
		}
		res, err := cuaRt.call(ctx, mapped.tool, mapped.args)
		if err != nil {
			return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateError, Error: "cua: " + err.Error()}
		}
		return cuaSnapshotResp(req, res, mapped, shotPath, txtPath)
	}
	res, err := cuaRt.call(ctx, mapped.tool, mapped.args)
	if err != nil {
		return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateError, Error: "cua: " + err.Error()}
	}
	// browser-state/bprepare：应答提取 target_id/tab_id 存绑定；
	// bprepare --isolated 只给 prepared_pid（驱动自建浏览器进程）——存下供
	// 后续 browser-state 注入 pid 完成绑定。
	if mapped.browserBind && res.StructuredContent != nil {
		if f, ok := res.StructuredContent["prepared_pid"].(float64); ok && f > 0 {
			cuaRt.setBrowserPid(int(f))
		}
		found := map[string]string{}
		cuaFindStrKeys(res.StructuredContent,
			map[string]bool{"target_id": true, "tab_id": true, "active_tab_id": true}, found, 0)
		tab := found["tab_id"]
		if tab == "" {
			tab = found["active_tab_id"]
		}
		if found["target_id"] != "" {
			cuaRt.setBrowserBinding(found["target_id"], tab)
		}
	}
	// 应答整形：text 拼接 + structuredContent 紧凑 JSON 一并带出（驱动的 text
	// 只是摘要，窗口列表/剪贴板文本等正文在 structuredContent；超 256KB 截断
	// 防 token 爆炸）；内联截图（如出现）落盘换路径。snapshot 已特化返回。
	var texts []string
	filePath := ""
	imgIdx := 0
	for _, item := range res.Content {
		switch {
		case item.Type == "text":
			texts = append(texts, item.Text)
		case item.Type == "image" && item.Data != "":
			data, derr := base64.StdEncoding.DecodeString(item.Data)
			if derr != nil {
				continue
			}
			cuaDir := filepath.Join(workDir, ".cua")
			if err := os.MkdirAll(cuaDir, 0o700); err != nil {
				continue
			}
			p := filepath.Join(cuaDir, fmt.Sprintf("snap-%d-%d.png", time.Now().UnixMilli(), imgIdx))
			imgIdx++
			if err := os.WriteFile(p, data, 0o600); err != nil {
				continue
			}
			filePath = p
			texts = append(texts, "[image] "+p)
		}
	}
	if res.StructuredContent != nil {
		sc, _ := json.Marshal(res.StructuredContent)
		const cap = 256 * 1024
		if len(sc) > cap {
			sc = append(sc[:cap], []byte("... (truncated)")...)
		}
		texts = append(texts, string(sc))
	}
	resp = &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateCompleted, Content: strings.Join(texts, "\n")}
	if filePath != "" {
		resp.Attrs = map[string]string{"path": filePath}
	}
	return resp
}

// runCuaPaste 执行非 ASCII 文本输入：写剪贴板 + 粘贴热键。
// 不走逐键合成（IME 会拦截/转候选），也不依赖目标控件的 AX 文本接口
// （Blender 等原生 app 不实现 AXSetAttribute 文本写入）。投递语义沿用原
// type 的 pid/window/delivery 参数。
func (c *Client) runCuaPaste(ctx context.Context, req *proto.ToolRequest, mapped *cuaCall) *proto.ToolResponse {
	fail := func(err error) *proto.ToolResponse {
		return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateError, Error: "cua: " + err.Error()}
	}
	if _, err := cuaRt.call(ctx, "clipboard_write", map[string]any{"text": mapped.paste}); err != nil {
		return fail(fmt.Errorf("paste: clipboard_write: %w", err))
	}
	args := make(map[string]any, len(mapped.args)+2)
	for k, v := range mapped.args {
		args[k] = v
	}
	args["key"] = "v"
	args["modifiers"] = []string{pasteModifier()}
	res, err := cuaRt.call(ctx, "press_key", args)
	if err != nil {
		return fail(fmt.Errorf("paste: %s+v: %w", pasteModifier(), err))
	}
	texts := []string{fmt.Sprintf("pasted %d char(s) via clipboard (%s+v)", len([]rune(mapped.paste)), pasteModifier())}
	for _, item := range res.Content {
		if item.Type == "text" && item.Text != "" {
			texts = append(texts, item.Text)
		}
	}
	if res.StructuredContent != nil {
		sc, _ := json.Marshal(res.StructuredContent)
		const maxLen = 64 * 1024
		if len(sc) > maxLen {
			sc = append(sc[:maxLen], []byte("... (truncated)")...)
		}
		texts = append(texts, string(sc))
	}
	return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateCompleted, Content: strings.Join(texts, "\n")}
}

// ---- snapshot 特化：正文落盘 + 返回精简 ----

// cuaSnapshotResp 组装 snapshot 应答：
//   - 正文（tree_markdown + elements JSON 全量）落盘 <shotPath 同名 .txt>；
//   - 返回精简：摘要头（窗口/bounds/元素数/routes）+ 顶层元素清单
//     （depth<=3，最多 60 行，保 --token/--x --y 可行动性）+ 正文路径；
//   - --grep：返回命中行 + token 图例（行为不变），全量正文同样落盘；
//   - --png：截图另以 image_data（data URI）附返回（服务端统一落盘为模型视觉
//     输入），[image] 路径行与 Attrs path 同步；截图失败显式报错（用户显式
//     请求，不应静默降级成无图）。
//
// 深度细节（菜单递归展开等）进 txt 不占返回 token，需要时 fs 读取。
func cuaSnapshotResp(req *proto.ToolRequest, res *mcpResult, mapped *cuaCall, shotPath, txtPath string) *proto.ToolResponse {
	sc := res.StructuredContent
	// 正文素材：tree_markdown（structuredContent 优先，缺失回退驱动 text 项）
	tree := ""
	if sc != nil {
		if t, ok := sc["tree_markdown"].(string); ok {
			tree = t
		}
	}
	if tree == "" {
		for _, item := range res.Content {
			if item.Type == "text" {
				tree = item.Text
				break
			}
		}
	}
	elements := cuaSnapshotElements(sc)
	// 正文落盘（失败暴露：截图已写而正文丢失会让成对契约破损）
	if tree != "" || len(elements) > 0 {
		if err := os.WriteFile(txtPath, []byte(cuaSnapshotDoc(sc, tree, elements)), 0o600); err != nil {
			return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateError,
				Error: "cua: write snapshot txt: " + err.Error()}
		}
	}

	var out strings.Builder
	// 摘要头：标题/pid/window/bounds/元素数/snapshot id
	title := ""
	if len(elements) > 0 {
		title, _ = elements[0]["label"].(string)
	}
	fmt.Fprintf(&out, "cua snapshot: %s (pid=%v window=%v)\n", title, scNum(sc, "pid"), scNum(sc, "window_id"))
	if wb, ok := sc["window_bounds"].(map[string]any); ok {
		fmt.Fprintf(&out, "bounds=%sx%s@%s,%s  elements=%v/%v  snapshot=%v\n",
			cuaBoundsVal(wb, "w", "width"), cuaBoundsVal(wb, "h", "height"),
			cuaBoundsVal(wb, "x", "x"), cuaBoundsVal(wb, "y", "y"),
			scNum(sc, "element_count"), scNum(sc, "total_element_count"), scVal(sc, "snapshot_id"))
	} else {
		// 结构化缺 window_bounds（异常应答）：补基础行不丢信息
		fmt.Fprintf(&out, "elements=%v/%v  snapshot=%v\n",
			scNum(sc, "element_count"), scNum(sc, "total_element_count"), scVal(sc, "snapshot_id"))
	}
	if degraded, _ := sc["degraded"].(bool); degraded {
		// 驱动 degraded（如 AX 未解析空树）：带出原因，避免 AI 误判为空窗口
		fmt.Fprintf(&out, "degraded: %s\n", truncStr(scVal(sc, "degraded_reason"), 240))
	}
	if routes := cuaSnapshotRoutes(sc); routes != "" {
		out.WriteString(routes + "\n")
	}

	if mapped.grep != "" {
		// grep：命中行 + token 图例（与旧行为一致），全量正文在 txt
		if filtered, idxs := grepTree(tree, mapped.grep, mapped.grepCtx); filtered != "" {
			out.WriteString(filtered)
			out.WriteString(cuaTokenLegend(elements, idxs))
		} else {
			fmt.Fprintf(&out, "[grep %q: 0 命中]", mapped.grep)
		}
	} else {
		// 语义摘要：结构 + 界面文本 + 可操作控件（带 token）+ 菜单折叠
		out.WriteString(cuaSnapshotSummary(elements, tree))
	}
	if mapped.png {
		out.WriteString("[image] " + shotPath + "\n")
	}
	out.WriteString("[text] " + txtPath + "\n")

	resp := &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateCompleted, Content: out.String()}
	// --png：截图转 image_data（data URI）附返回，服务端统一落盘为模型视觉输入；
	// 截图失败显式报错（用户显式请求，不应静默降级成无图）。
	if mapped.png {
		data, rerr := os.ReadFile(shotPath)
		if rerr != nil {
			return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateError,
				Error: "cua: snapshot --png: read screenshot: " + rerr.Error()}
		}
		dataURI, note, err := vcore.EncodeImageData(data, "image/png")
		if err != nil {
			return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateError,
				Error: "cua: snapshot --png: " + err.Error()}
		}
		resp.Attrs = map[string]string{"path": shotPath}
		resp.Attrs["image_data"] = dataURI
		if note != "" {
			resp.Attrs["image_compressed"] = note
		}
	}
	return resp
}

// cuaSnapshotDoc 生成 snapshot 正文（txt）：元信息头 + tree_markdown + elements JSON 全量。
func cuaSnapshotDoc(sc map[string]any, tree string, elements []map[string]any) string {
	var b strings.Builder
	b.WriteString("# cua snapshot 正文（tree_markdown + elements JSON 全量）\n")
	fmt.Fprintf(&b, "pid=%v window=%v snapshot=%v\n", scNum(sc, "pid"), scNum(sc, "window_id"), scVal(sc, "snapshot_id"))
	if wb, ok := sc["window_bounds"].(map[string]any); ok {
		fmt.Fprintf(&b, "bounds=%sx%s@%s,%s  elements=%v/%v  scale=%v\n",
			cuaBoundsVal(wb, "w", "width"), cuaBoundsVal(wb, "h", "height"),
			cuaBoundsVal(wb, "x", "x"), cuaBoundsVal(wb, "y", "y"),
			scNum(sc, "element_count"), scNum(sc, "total_element_count"), scNum(sc, "screenshot_scale"))
	}
	if degraded, _ := sc["degraded"].(bool); degraded {
		fmt.Fprintf(&b, "degraded: %s\n", scVal(sc, "degraded_reason"))
	}
	if tree != "" {
		b.WriteString("\n## tree_markdown\n" + tree + "\n")
	}
	if len(elements) > 0 {
		if js, err := json.Marshal(elements); err == nil {
			b.WriteString("\n## elements (JSON)\n" + string(js) + "\n")
		}
	}
	return b.String()
}

// cuaBoundsVal 取 bounds 的宽/高/坐标：驱动版本间键名可能为 w/h 或 width/height。
func cuaBoundsVal(m map[string]any, keyA, keyB string) string {
	if v, ok := m[keyA]; ok {
		return fmt.Sprintf("%v", v)
	}
	if v, ok := m[keyB]; ok {
		return fmt.Sprintf("%v", v)
	}
	return "?"
}

// truncStr 截断长字符串（degraded_reason 等可能很长），按 rune 计数。
func truncStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// cuaSnapshotElements 提取 structuredContent 的 elements 数组（元素 map 视图）。
func cuaSnapshotElements(sc map[string]any) []map[string]any {
	if sc == nil {
		return nil
	}
	els, _ := sc["elements"].([]any)
	out := make([]map[string]any, 0, len(els))
	for _, e := range els {
		if em, ok := e.(map[string]any); ok {
			out = append(out, em)
		}
	}
	return out
}

// cuaElemLine 渲染单个元素的精简行：- [token] role (x,y WxH) "label"。
func cuaElemLine(e map[string]any) string {
	var b strings.Builder
	if tok, _ := e["element_token"].(string); tok != "" {
		b.WriteString("- [" + tok + "] ")
	} else {
		b.WriteString("- ")
	}
	if role, _ := e["role"].(string); role != "" {
		b.WriteString(role)
	}
	if fr, ok := e["frame"].(map[string]any); ok {
		fmt.Fprintf(&b, " (%v,%v %vx%v)", fr["x"], fr["y"], fr["w"], fr["h"])
	}
	if label, _ := e["label"].(string); label != "" {
		fmt.Fprintf(&b, " %q", label)
	}
	return b.String()
}

// 语义摘要的 role 分组（提炼而非堆砌：有 label 才有信息量）。
var cuaInfoRoles = map[string]bool{
	"AXStaticText": true, "AXHeading": true, "AXCell": true, "AXHelpTag": true,
}

var cuaControlRoles = map[string]bool{
	"AXButton": true, "AXCheckBox": true, "AXRadioButton": true, "AXPopUpButton": true,
	"AXTextField": true, "AXComboBox": true, "AXLink": true, "AXSlider": true,
	"AXIncrementor": true, "AXDisclosureTriangle": true,
}

var cuaStructRoles = map[string]bool{
	"AXWindow": true, "AXOutline": true, "AXToolbar": true, "AXGroup": true,
	"AXList": true, "AXTable": true, "AXTabGroup": true, "AXSplitGroup": true,
	"AXScrollArea": true, "AXSheet": true, "AXMenuBar": true, "AXDialog": true,
}

// cuaSnapshotSummary 提炼窗口语义摘要（替代按 depth 直列）：
//   - 结构：布局容器（窗口/边栏/工具栏/列表…），≤15 行；无 label 的 AXRow
//     折叠为统计行；
//   - 文本：界面静态文本/输入框占位（边栏项/列表项/说明），≤40 行，均带
//     坐标（可定位/看空间结构；elements 有 frame 的带，tree 无索引行无坐标
//     数据只列文本）；
//   - 可操作：带 element_token 的控件（按钮/输入框/勾选），≤40 行——可直接
//     用 token 行动；
//   - 菜单项：折叠为一行示例（完整在 txt）。
//
// 深度不限（语义优先），无 label 的控件/行不入摘要，完整明细在 txt。
func cuaSnapshotSummary(elements []map[string]any, tree string) string {
	if len(elements) == 0 && tree == "" {
		return "（无 AX 元素：树为空或被拒，见 degraded 行）\n"
	}
	// 文本收集：同 label 有坐标版优先（先到无坐标、后到补 frame），按 depth 排序输出。
	type textItem struct {
		label string
		frame string
		depth float64
	}
	texts := []textItem{}
	byLabel := map[string]int{}
	addText := func(label, frame string, depth float64) {
		if label == "" {
			return
		}
		if idx, ok := byLabel[label]; ok {
			if texts[idx].frame == "" && frame != "" {
				texts[idx].frame = frame // 补坐标（有坐标版更优）
			}
			return
		}
		byLabel[label] = len(texts)
		texts = append(texts, textItem{label: label, frame: frame, depth: depth})
	}
	menuLabels, menuSeen := []string{}, map[string]bool{}
	addMenu := func(s string) {
		if s != "" && !menuSeen[s] {
			menuSeen[s] = true
			menuLabels = append(menuLabels, s)
		}
	}
	// 树解析：无 [N] 索引行 = 非行动元素（elements 数组不含），文本/菜单从这补全；
	// 缩进空格数/2 近似 depth（仅排序用，树行无 frame 数据）
	for _, ln := range strings.Split(tree, "\n") {
		indent := len(ln) - len(strings.TrimLeft(ln, " "))
		t := strings.TrimLeft(ln, " ")
		if !strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "- [") {
			continue // 非树行；有 [N] 的行与 elements 一一对应（label 已从 elements 提取）
		}
		body := strings.TrimLeft(strings.TrimPrefix(t, "-"), " ")
		role := ""
		if i := strings.IndexAny(body, " \t"); i > 0 {
			role = body[:i]
		}
		// 无索引行的语义：= "value" 或 "label" 或 (label)
		label := ""
		if i := strings.Index(body, `"`); i >= 0 {
			if j := strings.Index(body[i+1:], `"`); j >= 0 {
				label = body[i+1 : i+1+j]
			}
		} else if i := strings.Index(body, "("); i >= 0 {
			if j := strings.Index(body[i+1:], ")"); j >= 0 {
				label = body[i+1 : i+1+j]
			}
		}
		if label == "" {
			continue
		}
		if role == "AXMenuItem" || role == "AXMenuBarItem" {
			addMenu(label)
		} else if cuaInfoRoles[role] || role == "AXTextField" {
			addText(label, "", float64(indent)/2)
		}
	}
	// elements：结构/控件/文本补充（有 token 的数组元素）
	var structs, controls []string
	rowCount := 0
	for _, e := range elements {
		role, _ := e["role"].(string)
		label := cuaElemLabel(e)
		depth, _ := e["depth"].(float64)
		switch {
		case role == "AXRow":
			rowCount++
		case role == "AXMenuItem" || role == "AXMenuBarItem":
			addMenu(label)
		case cuaInfoRoles[role] && label != "":
			addText(label, cuaFrameStr(e), depth)
		case cuaControlRoles[role] && label != "":
			controls = append(controls, cuaElemLine(e))
		case cuaStructRoles[role]:
			line := role
			if label != "" {
				line += ` "` + label + `"`
			}
			structs = append(structs, line)
		}
	}
	var b strings.Builder
	const structCap, textCap, controlCap = 15, 40, 40
	b.WriteString("结构:\n")
	for i, s := range structs {
		if i >= structCap {
			fmt.Fprintf(&b, "…（结构容器 %d 个，仅列前 %d）\n", len(structs), structCap)
			break
		}
		b.WriteString("- " + s + "\n")
	}
	if rowCount > 0 {
		fmt.Fprintf(&b, "- 列表行 AXRow ×%d（内容在其子文本，详见 txt）\n", rowCount)
	}
	b.WriteString("文本:\n")
	sort.SliceStable(texts, func(i, j int) bool { return texts[i].depth < texts[j].depth })
	for i, t := range texts {
		if i >= textCap {
			fmt.Fprintf(&b, "…（文本 %d 条，仅列前 %d）\n", len(texts), textCap)
			break
		}
		if t.frame != "" {
			b.WriteString("- " + t.frame + " \"" + t.label + "\"\n")
		} else {
			b.WriteString("- \"" + t.label + "\"\n")
		}
	}
	b.WriteString("可操作 (token 可直接用):\n")
	for i, c := range controls {
		if i >= controlCap {
			fmt.Fprintf(&b, "…（控件 %d 个，仅列前 %d）\n", len(controls), controlCap)
			break
		}
		b.WriteString(c + "\n")
	}
	if len(menuLabels) > 0 {
		ex := menuLabels
		if len(ex) > 5 {
			ex = ex[:5]
		}
		fmt.Fprintf(&b, "菜单项 ×%d（示例: %s，完整在 txt）\n", len(menuLabels), strings.Join(ex, " / "))
	}
	return b.String()
}

// cuaFrameStr 渲染元素 frame：(x,y w,h)；无 frame 或全 0（驱动未填）返回空串。
func cuaFrameStr(e map[string]any) string {
	fr, ok := e["frame"].(map[string]any)
	if !ok {
		return ""
	}
	x, _ := fr["x"].(float64)
	y, _ := fr["y"].(float64)
	w, _ := fr["w"].(float64)
	h, _ := fr["h"].(float64)
	if x == 0 && y == 0 && w == 0 && h == 0 {
		return ""
	}
	return fmt.Sprintf("(%v,%v %vx%v)", x, y, w, h)
}

// cuaElemLabel 取元素的语义文本：label → value → description。
func cuaElemLabel(e map[string]any) string {
	for _, k := range []string{"label", "value", "description"} {
		if v, _ := e[k].(string); v != "" {
			return v
		}
	}
	return ""
}

// cuaTokenLegend 生成命中行元素索引→token 图例（保可行动性，最多 40 行）。
func cuaTokenLegend(elements []map[string]any, idxs []int) string {
	if len(idxs) == 0 {
		return ""
	}
	tokByIdx := map[int]string{}
	for _, e := range elements {
		if i, ok := e["element_index"].(float64); ok {
			if tok, _ := e["element_token"].(string); tok != "" {
				tokByIdx[int(i)] = tok
			}
		}
	}
	var lg strings.Builder
	lg.WriteString("[element_token 图例]\n")
	for i, idx := range idxs {
		if i >= 40 {
			fmt.Fprintf(&lg, "  …（共 %d 个，仅列前 40）\n", len(idxs))
			break
		}
		if tok := tokByIdx[idx]; tok != "" {
			fmt.Fprintf(&lg, "  [%d] %s\n", idx, tok)
		}
	}
	return lg.String()
}

// cuaSnapshotRoutes 渲染感知通道可用性（routes 名=状态）。
func cuaSnapshotRoutes(sc map[string]any) string {
	bi, _ := sc["background_input"].(map[string]any)
	routes, _ := bi["routes"].([]any)
	if len(routes) == 0 {
		return ""
	}
	parts := make([]string, 0, len(routes))
	for _, r := range routes {
		rm, _ := r.(map[string]any)
		if name, _ := rm["route"].(string); name != "" {
			if status, _ := rm["status"].(string); status != "" {
				parts = append(parts, name+"="+status)
			} else {
				parts = append(parts, name)
			}
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "routes: " + strings.Join(parts, ", ")
}

// scNum/scVal：structuredContent 数字/字符串取值（缺失返回 0/""）。
func scNum(sc map[string]any, k string) string {
	if sc == nil {
		return ""
	}
	if v, ok := sc[k]; ok {
		return fmt.Sprintf("%v", v)
	}
	return ""
}

func scVal(sc map[string]any, k string) string {
	if sc == nil {
		return ""
	}
	if v, ok := sc[k].(string); ok {
		return v
	}
	return ""
}
