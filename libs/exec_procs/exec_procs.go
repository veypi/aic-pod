// Package exec_procs 是 exec 子进程统一托管（§5.8/§5.9）。
//
// 凡是 exec 命令创建的子进程（host 程序执行、agent-browser CLI）都经此管理：
//   - 创建子进程，stdout+stderr 合并重定向到日志文件（路径由调用方指定：
//     host = {tmp}/aic/{sid}/{msg_id}.log，cloud browser = $SESSION/.exec/{msg_id}.log）
//   - 请求超时 → 自动后台化（进程继续运行），返回 background=true + id
//   - 输出超限 → 返回前 MaxLines 行 + truncated + path（AI 可续读完整输出）
//   - bg_list / bg_wait / bg_kill 统一管理
//
// Manager 为每会话一个（host 端 Client 持有；cloud 端 exec 工具持有），
// 后台条目保留至进程终结（含退出码），供 bg_wait 取结果。
package exec_procs

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// MaxLines 是返回内容的最大行数（与 exec 统一截断语义一致）。
const MaxLines = 1000

// DefaultExecTimeout 是后台进程的自有超时（§5.8：默认 30m）。
const DefaultExecTimeout = 30 * time.Minute

// Entry 是一个托管的后台条目（进程结束后保留，供 bg_wait 取结果）。
type Entry struct {
	ID       string // {host}:{sid}:{op_id} 或 {msg_id}
	Command  string
	LogPath  string // 输出落盘路径
	Started  time.Time
	Timeout  time.Duration // 进程自有超时（bg_list 展示 remaining）
	ExitCode int

	pid    int // 0 = 托管任务（非子进程，kill 仅 cancel）
	cancel context.CancelFunc
	done   chan struct{}
	runErr error // 托管任务体的错误（同步路径原样返回，日志同写）
}

// Done 报告进程是否已终结。
func (e *Entry) Done() bool {
	select {
	case <-e.done:
		return true
	default:
		return false
	}
}

// PID 返回进程 ID（bg_list/bg_kill 展示用）。
func (e *Entry) PID() int { return e.pid }

// Result 是 Start/Wait 的统一返回（§5.9：正常 / 超时转后台同源，从日志文件读）。
type Result struct {
	Content    string
	Lines      int
	Truncated  bool
	ExitCode   int
	Background bool // true = 已转后台（请求超时，进程继续运行）
	ID         string
	LogPath    string
}

// StartOptions 是 Start 的入参。
type StartOptions struct {
	ID      string   // 后台条目 ID（{host}:{sid}:{op_id} 或 msgID）
	Command string   // 展示名（bg_list）
	LogPath string   // 输出落盘路径（父目录自动创建）
	Workdir string   // 进程 cwd（空 = 继承）；兼作沙箱 workspace-write 的可写根
	Exec    []string // argv：Exec[0] = 程序名
	// Level 是本次调用的授予等级（§2.4/§5.10 沙箱 profile 选择）：
	// 1 = read-only 沙箱；2/3/4/9 = workspace-write 沙箱；
	// 0 = 未设置/异常值，按 read-only 兜底（fail-closed——
	// host 外部调用的 level 0 已被 dispatch 拒绝，
	// 到这里的 0 只会是调用方 bug，宁可过紧也不可裸跑）。
	// 注意：LevelApproved(9) 只是「审批通过」的等级语义，不免沙箱——
	// 免沙箱唯一通道是 NoSandbox。
	Level int
	// NoSandbox 是免沙箱执行标记（§5.10），唯二合法来源：
	//   - 内部调用方（cloud/host 端 browser，由执行环境自身管控）；
	//   - 外部请求显式携带 nosandbox 且经人工审批（required Critical(4)
	//     ⇒ 必审批；审批本身不免沙箱，仅放行该标记）。
	NoSandbox bool
}

// Manager 是 exec 子进程托管管理器（每 session 一个）。
type Manager struct {
	mu          sync.Mutex
	execTimeout time.Duration // 后台进程自有超时
	tasks       map[string]*Entry
	// NoSandbox 全局免沙箱（§5.10）：置 true 后所有 Start 调用跳过沙箱包装
	// （与 StartOptions.NoSandbox 同效）。配置源：cfg.Options.NoSandbox。
	NoSandbox bool
}

// NewManager 创建管理器。execTimeout 为后台进程自有超时（0 = DefaultExecTimeout）。
func NewManager(execTimeout time.Duration) *Manager {
	if execTimeout <= 0 {
		execTimeout = DefaultExecTimeout
	}
	return &Manager{execTimeout: execTimeout, tasks: map[string]*Entry{}}
}

// SetExecTimeout 更新后台进程超时（配置保存后生效，不影响已在运行的 Entry）。
func (m *Manager) SetExecTimeout(d time.Duration) {
	if d <= 0 {
		d = DefaultExecTimeout
	}
	m.mu.Lock()
	m.execTimeout = d
	m.mu.Unlock()
}

// Start 启动子进程并托管（§5.9）：
//   - stdout+stderr 合并重定向到 opts.LogPath
//   - ctx 超时 → 自动后台化：进程继续运行，返回 Background=true + ID
//   - 正常完成 → 返回日志前 MaxLines 行 + ExitCode
func (m *Manager) Start(ctx context.Context, opts StartOptions) (*Result, error) {
	if len(opts.Exec) == 0 || strings.TrimSpace(opts.Exec[0]) == "" {
		return nil, fmt.Errorf("exec: program name is required")
	}
	if _, err := exec.LookPath(opts.Exec[0]); err != nil {
		return nil, fmt.Errorf("exec: unknown action %q", opts.Exec[0])
	}

	logDir := filepath.Dir(opts.LogPath)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, fmt.Errorf("exec: create log dir: %v", err)
	}
	f, err := os.Create(opts.LogPath)
	if err != nil {
		return nil, fmt.Errorf("exec: create log file: %v", err)
	}

	// 沙箱包装（§5.10）：未显式免沙箱（NoSandbox）且全局未禁用（m.NoSandbox）
	// 的进程调用一律进沙箱——审批通过（9）也不例外，免沙箱只能由显式
	// nosandbox 请求 + 审批或全局 no_sandbox 配置获得；
	// 无可用后端时 fail-closed 返回错误（命令不执行，绝不静默裸跑）。
	execArgv := opts.Exec
	var plan launchPlan
	if !opts.NoSandbox && !m.NoSandbox && len(opts.Exec) > 0 {
		var err error
		plan, err = planConfined(opts.Level, opts.Workdir, opts.Exec)
		if err != nil {
			f.Close()
			return nil, err
		}
		execArgv = plan.argv
	}

	// 后台 context：不继承请求 deadline，进程自有超时（bg_kill 或自然终结时释放）
	m.mu.Lock()
	timeout := m.execTimeout
	m.mu.Unlock()
	bgCtx, bgCancel := context.WithTimeout(context.Background(), timeout)
	cmd := exec.CommandContext(bgCtx, execArgv[0], execArgv[1:]...)
	cmd.Dir = opts.Workdir
	if plan.env != nil {
		cmd.Env = mergeEnv(plan.env)
	}
	// Windows 上经逐行转码（GBK→UTF-8）后落盘，其余平台原样直写
	out := newOutputWriter(f)
	cmd.Stdout = out
	cmd.Stderr = out
	SetSysProcAttr(cmd)
	if plan.token != 0 {
		if err := applyToken(cmd, plan.token); err != nil {
			f.Close()
			bgCancel()
			closeToken(plan.token)
			if plan.cleanup != nil {
				plan.cleanup()
			}
			return nil, fmt.Errorf("exec: apply token: %v", err)
		}
	}

	if err := cmd.Start(); err != nil {
		f.Close()
		bgCancel()
		closeToken(plan.token)
		if plan.cleanup != nil {
			plan.cleanup()
		}
		return nil, fmt.Errorf("exec: %v", err)
	}
	// spawn 成功后令牌句柄可释放（子进程持有副本）
	closeToken(plan.token)

	e := &Entry{
		ID:      opts.ID,
		Command: opts.Command,
		LogPath: opts.LogPath,
		Started: time.Now(),
		Timeout: m.execTimeout,
		pid:     cmd.Process.Pid,
		cancel:  bgCancel,
		done:    make(chan struct{}),
	}
	m.mu.Lock()
	m.tasks[opts.ID] = e
	m.mu.Unlock()

	go func() {
		runErr := cmd.Wait()
		if c, ok := out.(interface{ Close() error }); ok {
			_ = c.Close() // flush 行尾残留（Windows 转码器）
		}
		f.Close()
		bgCancel()
		// 沙箱清理（windows：私有临时目录删除，ACE 随目录消失）
		if plan.cleanup != nil {
			plan.cleanup()
		}
		e.ExitCode = 0
		if ee, ok := runErr.(*exec.ExitError); ok {
			e.ExitCode = ee.ExitCode()
		} else if runErr != nil {
			e.ExitCode = -1
		}
		close(e.done)
	}()

	select {
	case <-e.done:
		return m.readResult(e, false), nil
	case <-ctx.Done():
		// 请求超时 → 自动后台化：进程继续运行
		return m.readResult(e, true), nil
	}
}

// TaskOptions 是 StartTask 的入参（托管任务：非子进程的任务体，如 curl 抓取）。
type TaskOptions struct {
	ID      string // 后台条目 ID
	Command string // 展示名（bg_list）
	LogPath string // 输出落盘路径（父目录自动创建）
	// Run 是任务体：输出写 out（日志文件）；返回 error = 任务失败
	// （同步路径原样返回该错误；后台路径错误写入日志，bg_wait 可见）。
	Run func(ctx context.Context, out io.Writer) error
}

// StartTask 启动托管任务（§5.9 与子进程同一套语义）：
//   - Run 的输出重定向到 opts.LogPath
//   - ctx 超时 → 自动后台化：任务继续运行，返回 Background=true + ID
//   - 同步完成且 Run 返回 error → 原样返回该错误（ExitCode=-1）
//   - 正常完成 → 返回日志前 MaxLines 行
func (m *Manager) StartTask(ctx context.Context, opts TaskOptions) (*Result, error) {
	if opts.Run == nil {
		return nil, fmt.Errorf("exec: task body is required")
	}
	logDir := filepath.Dir(opts.LogPath)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, fmt.Errorf("exec: create log dir: %v", err)
	}
	f, err := os.Create(opts.LogPath)
	if err != nil {
		return nil, fmt.Errorf("exec: create log file: %v", err)
	}

	m.mu.Lock()
	timeout := m.execTimeout
	m.mu.Unlock()
	bgCtx, bgCancel := context.WithTimeout(context.Background(), timeout)
	e := &Entry{
		ID:      opts.ID,
		Command: opts.Command,
		LogPath: opts.LogPath,
		Started: time.Now(),
		Timeout: timeout,
		pid:     0, // 非子进程：kill 仅 cancel
		cancel:  bgCancel,
		done:    make(chan struct{}),
	}
	m.mu.Lock()
	m.tasks[opts.ID] = e
	m.mu.Unlock()

	go func() {
		runErr := opts.Run(bgCtx, f)
		if runErr != nil {
			e.runErr = runErr
			e.ExitCode = -1
			// 后台路径同步路径都能从日志看到失败原因
			fmt.Fprintf(f, "task failed: %s\n", runErr)
		}
		f.Close()
		bgCancel()
		close(e.done)
	}()

	select {
	case <-e.done:
		if e.runErr != nil {
			return nil, e.runErr
		}
		return m.readResult(e, false), nil
	case <-ctx.Done():
		return m.readResult(e, true), nil
	}
}

// readResult 从日志文件读前 MaxLines 行（§5.9：正常/超时同源）。
func (m *Manager) readResult(e *Entry, background bool) *Result {
	content, lines, truncated := readHeadLines(e.LogPath, MaxLines)
	return &Result{
		Content:    content,
		Lines:      lines,
		Truncated:  truncated,
		ExitCode:   e.ExitCode,
		Background: background,
		ID:         e.ID,
		LogPath:    e.LogPath,
	}
}

// List 返回仍在运行的后台条目（§5.8 bg_list 语义：已终结的不在列表）。
func (m *Manager) List() []*Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Entry, 0, len(m.tasks))
	for _, e := range m.tasks {
		if !e.Done() {
			out = append(out, e)
		}
	}
	return out
}

// Wait 等待后台条目结果（§5.8 bg_wait）：
//   - wait 时间内完成 → 完整结果
//   - 超时 → Background=true（可再次 bg_wait）
//   - 条目不存在或已终结但不在注册表 → error no such background process
func (m *Manager) Wait(ctx context.Context, id string, wait time.Duration) (*Result, error) {
	e := m.get(id)
	if e == nil {
		return nil, fmt.Errorf("no such background process: %s", id)
	}
	select {
	case <-e.done:
		return m.readResult(e, false), nil
	case <-time.After(wait):
		return m.readResult(e, true), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Kill 终止后台条目（§5.8 bg_kill：进程组 SIGTERM → 5s SIGKILL + cancel）。
// 已终结的条目报 no such background process（bg_list 语义一致）。
func (m *Manager) Kill(id string) error {
	e := m.get(id)
	if e == nil || e.Done() {
		return fmt.Errorf("no such background process: %s", id)
	}
	killEntry(e)
	return nil
}

func (m *Manager) get(id string) *Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tasks[id]
}

// Get 返回指定 id 的条目（不存在返回 nil）。
func (m *Manager) Get(id string) *Entry { return m.get(id) }

// readHeadLines 读取日志前 maxLines 行（§5.9：与 exec 统一截断语义一致）。
func readHeadLines(path string, maxLines int) (content string, lines int, truncated bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	var b strings.Builder
	for lines < maxLines && scanner.Scan() {
		lines++
		b.WriteString(scanner.Text())
		b.WriteByte('\n')
	}
	if scanner.Scan() {
		truncated = true
	}
	if err := scanner.Err(); err != nil {
		truncated = true
	}
	return b.String(), lines, truncated
}
