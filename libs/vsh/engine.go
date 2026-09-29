// Package vsh 是 aic-pod 的 vsh 引擎集成层（glue，cloud/host 共用）。
//
// 职责：Engine 生命周期（Runtime 单例 + per-exec 派生会话 + limits/墙钟 +
// panic recover + 后台任务登记表）、FS 适配器（fs_ufs.go / fs_host.go，唯一
// 进程内路径权威）、NetClient（netclient.go）、静态分析（analyze.go）、
// 平台命令（cmds.go）、原生命令包装器（native.go）。
//
// hosts-vsh-redesign 要点：
//   - 每次 exec 使用独立派生会话（共享 backing FS，独立内存层）——脚本执行
//     不持有跨调用的会话锁，bg 查询/取消与组合脚本永不被另一执行阻塞
//     （不靠单指令特判）。跨 exec 的 cwd/变量不持久（外层每次显式传 workdir）。
//   - 可信身份与审批事实经 ctx 传递（Owner/SessionKey/GrantApproved/NoSandbox），
//     不从脚本可修改的 env（AIC_VSH_*）取授权信息。
//   - bg 任务唯一来源是 exec 前台等待超时（Adopt 登记已运行执行）；任务表
//     不再启动执行、不保存输出缓冲。
//
// 引擎路径权威分工（v4.1 强制项）：引擎 Policy 的 SymlinkMode 必须显式覆盖为
// 放行型（SymlinkFollow）——默认 SymlinkDeny 会在 AllowPath 先于 FS 适配器
// 拦截 symlink 穿越，与适配器 canonicalize-then-check 语义冲突；FS 适配器
// 是唯一进程内路径权威（vbox 规则表门在适配器内）。
package vsh

import (
	"context"
	"fmt"
	"io"
	"maps"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/veypi/vbox"
	vshcore "github.com/veypi/vsh"
	"github.com/veypi/vsh/commands"
	"github.com/veypi/vsh/contrib/jq"
	gbfs "github.com/veypi/vsh/fs"
	vshnet "github.com/veypi/vsh/network"
	vshpolicy "github.com/veypi/vsh/policy"
	"github.com/veypi/vsh/trace"
)

// limits 定稿（已拍板）。
const (
	MaxStdoutBytes       = 8 << 20 // 8 MiB
	MaxStderrBytes       = 1 << 20 // 1 MiB
	MaxFileBytes         = 64 << 20
	MaxCommandCount      = 10000
	MaxLoopIterations    = 10000
	MaxGlobOperations    = 100000
	MaxSubstitutionDepth = 50
	// MaxForegroundWait 前台等待上限（page 端 180s 由 exec 工具层 cap）。
	MaxForegroundWait = 300 * time.Second
	// BackgroundWallClock 执行墙钟：到期以 124 终止（30min）。
	BackgroundWallClock = 30 * time.Minute

	// MaxRunningTasksPerOwner 单 owner 同时运行任务上限（容量闸）：单任务只有
	// 30min 墙钟不限并发——fan-out/多会话并发可占满全部核。超额快速拒绝
	//（排队本身是 DoS 放大器）。
	MaxRunningTasksPerOwner = 4
	// MaxRetainedFinishedTasks 完成任务表保留上限（超出逐出最旧）。
	MaxRetainedFinishedTasks = 64
)

// maxRunningTasksGlobal 全局同时运行任务上限：核数一半（下限 2）——即使全部
// 任务是 CPU 打满型也给同进程其他负载（消息/推理/网关）永远留一半核。
func maxRunningTasksGlobal() int {
	if n := runtime.NumCPU() / 2; n > 2 {
		return n
	}
	return 2
}

func engineLimits() vshpolicy.Limits {
	return vshpolicy.Limits{
		MaxStdoutBytes:       MaxStdoutBytes,
		MaxStderrBytes:       MaxStderrBytes,
		MaxFileBytes:         MaxFileBytes,
		MaxCommandCount:      MaxCommandCount,
		MaxLoopIterations:    MaxLoopIterations,
		MaxGlobOperations:    MaxGlobOperations,
		MaxSubstitutionDepth: MaxSubstitutionDepth,
	}
}

// EngineConfig 是 Engine 的装配参数。平台侧（aic / pod）注入每会话文件系统
// 工厂与平台命令依赖；策略源（vbox.Policy）经 FS 适配器 / NetClient 各自持有，
// 不经 Engine。
type EngineConfig struct {
	// NewSessionFS 构建每会话文件系统（cloud = UFS 直通 + 内存层 + jail；
	// host = OS backing）。入参为基会话键（派生后缀已剥离）；返回的 workDir
	// 是该会话的默认工作目录。
	NewSessionFS func(sessionKey string) (fsys gbfs.FileSystem, workDir string, err error)
	// Network 注入 NetClient（cloud 唯一网络权威）；nil = 不注册 curl。
	Network vshnet.Client
	// Platform 平台命令依赖（cmds.go）；零值可用（各命令降级为可读报错）。
	Platform PlatformDeps
	// CommandAllow 虚拟指令的执行规则门（host 接线 cfg exec 域）：
	// 命中已注册指令时调用，返回 false 即权限拒绝（不继续 fallback）。
	// nil = 全部放行（cloud；page 无原生能力另由端侧收口）。
	CommandAllow func(ctx context.Context, name string) bool
	// BaseEnv 每次 exec 注入的基础环境（HOME/PATH 钉死由调用方给；
	// 不跨 exec 持久，ExecRequest.Env 覆盖同名键）。
	BaseEnv map[string]string
	// LayoutEnv 覆盖 Runtime 级布局初始化环境（默认 layoutInitEnv：HOME 钉
	// 内存层 /tmp/.vsh-layout-home）。host 端无内存层，必须显式给可写布局
	// HOME 与 PATH（PATH 目录 = stub 写入目标，须在规则表可写区）。
	LayoutEnv map[string]string
	// BuiltinCommandDir 重写 shell 内置名（echo/bg/help…）的 stub 解析目录
	// （空 = vsh 默认 /bin，仅内存层文件系统可用）。host 端无内存层，必须
	// 指向真实 stub 目录（= LayoutEnv PATH 目录）。
	BuiltinCommandDir string
	// NativeFallback 命令 Registry 未命中时的兜底（host 原生适配器：
	// 按 PATH 查找并执行，执行期受命令规则与进程沙箱约束）。
	// nil = 未命中即 127。
	NativeFallback func(name string) (commands.Command, bool)
	// Logf 可选日志。
	Logf func(format string, args ...any)
}

// Engine 是 vsh Runtime 单例 + 后台任务登记表。
type Engine struct {
	cfg   EngineConfig
	rt    *vshcore.Runtime
	reg   *commands.Registry
	Tasks *TaskTable
}

// sessionFSKey 把每会话 FS 经 ctx 传给 Runtime 的 fs.Factory（Runtime 单例，
// Factory.New(ctx) 在 NewSession 时调用——以此把会话身份带进工厂）。
type sessionFSKey struct{}

// netSessionKey 把会话键经 ctx 传给 NetClient（规则表 per-session 快照，
// cloud 多用户进程不可用进程级并集——跨用户泄漏授权）。
type netSessionKey struct{}

// ownerKey 把任务归属（用户身份）经 ctx 透传。
type ownerKey struct{}

// grantApprovedKey 携带服务端确认过的 grant 审批事实（可信上下文；
// 不进入 argv/env——脚本不可修改）。
type grantApprovedKey struct{}

// noSandboxKey 携带本次执行的免沙箱选项（可信上下文）。
type noSandboxKey struct{}

// waitBudgetKey 携带前台等待截止（bg wait 的共享预算源）。
type waitBudgetKey struct{}

// execHandleKey 携带本次执行的句柄（bg wait 禁止等待自身用）。
type execHandleKey struct{}

// OwnerFromContext 取 Exec 注入的任务归属（无注入 = 空串匿名共池）。
func OwnerFromContext(ctx context.Context) string {
	o, _ := ctx.Value(ownerKey{}).(string)
	return o
}

// SessionFromContext 取 Exec 注入的会话键（NetClient 规则表快照源用；
// 无注入 = 空串——快照退化为无 temp 行的基表）。
func SessionFromContext(ctx context.Context) string {
	sid, _ := ctx.Value(netSessionKey{}).(string)
	return sid
}

// GrantApprovedFromContext 取本次执行的 grant 审批事实（默认 false）。
func GrantApprovedFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(grantApprovedKey{}).(bool)
	return v
}

// NoSandboxFromContext 取本次执行的免沙箱选项（默认 false）。
func NoSandboxFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(noSandboxKey{}).(bool)
	return v
}

// WaitBudgetRemaining 取剩余前台等待预算（bg wait 用）：
// 返回 min(剩余预算减 1 秒) 与 ok；无预算（后台执行/未注入）或预算耗尽
// 时 ok=false——此时 wait 只查询不阻塞。
func WaitBudgetRemaining(ctx context.Context) (time.Duration, bool) {
	deadline, ok := ctx.Value(waitBudgetKey{}).(time.Time)
	if !ok {
		return 0, false
	}
	remain := time.Until(deadline) - time.Second
	if remain <= 0 {
		return 0, false
	}
	return remain, true
}

// HandleFromContext 取本次执行的句柄（bg wait 禁止等待自身用）。
func HandleFromContext(ctx context.Context) *ExecHandle {
	h, _ := ctx.Value(execHandleKey{}).(*ExecHandle)
	return h
}

// layoutInitEnv 是 Runtime 级 BaseEnv：仅服务于 NewSession 的布局初始化
// （initializeSandboxLayout 用 r.cfg.BaseEnv 做 MkdirAll(HOME)/Chmod(/tmp)/写
// PATH stub）。HOME 钉进内存层（/tmp 前缀）——真实 HOME（cloud=/u/{uid}）
// 由平台每次 exec 经 ExecRequest.Env 注入（executionEnv 覆盖 BaseEnv），
// Runtime 单例因此可跨用户/会话共享（布局初始化不触碰 jail 内真实路径）。
// host 端无内存层：该 HOME 需落在规则表可写处。
var layoutInitEnv = map[string]string{
	"HOME": "/tmp/.vsh-layout-home",
	"PATH": "/usr/bin:/bin",
	"USER": "agent",
}

// NewEngine 装配 Runtime 单例：组合 Registry（内建 + jq + 平台命令）、静态
// Policy（SymlinkFollow 放行型 + §6 limits）、FS 工厂（每会话 ctx 路由）。
func NewEngine(cfg EngineConfig) (*Engine, error) {
	if cfg.NewSessionFS == nil {
		return nil, fmt.Errorf("vsh glue: NewSessionFS required")
	}
	reg := vshcore.DefaultRegistry()
	if err := jq.Register(reg); err != nil {
		return nil, fmt.Errorf("vsh glue: register jq: %w", err)
	}
	e := &Engine{cfg: cfg, reg: reg, Tasks: NewTaskTable()}
	// bg 指令需要任务登记表（cmds.go 不反向引用引擎，由此注入）。
	cfg.Platform.Tasks = e.Tasks
	if err := RegisterPlatformCommands(reg, cfg.Platform); err != nil {
		return nil, fmt.Errorf("vsh glue: register platform commands: %w", err)
	}
	pol := vshpolicy.NewStatic(&vshpolicy.Config{
		Limits: engineLimits(),
		// v4.1 强制项：放行型 symlink——FS 适配器 canonicalize-then-check
		// 是唯一进程内路径权威；命令准入由 Registry 收口（127），不经 Policy。
		SymlinkMode: vshpolicy.SymlinkFollow,
	})
	factory := gbfs.FactoryFunc(func(ctx context.Context) (gbfs.FileSystem, error) {
		fsys, _ := ctx.Value(sessionFSKey{}).(gbfs.FileSystem)
		if fsys == nil {
			return nil, fmt.Errorf("vsh glue: session filesystem missing (NewSessionFS not consulted)")
		}
		return fsys, nil
	})
	layoutEnv := cfg.LayoutEnv
	if layoutEnv == nil {
		layoutEnv = layoutInitEnv
	}
	// 解析兜底：Registry 未命中时交 NativeFallback（host 原生适配器）；
	// base 命中恒优先（registry 优先不动摇）。命中指令先过执行规则门
	//（CommandAllow），被拒返回权限错误命令——不能 fallback 绕过拒绝。
	var regIf commands.CommandRegistry = reg
	if cfg.NativeFallback != nil || cfg.CommandAllow != nil {
		regIf = fallbackRegistry{base: reg, fallback: cfg.NativeFallback, allow: cfg.CommandAllow}
	}
	opts := []vshcore.Option{
		vshcore.WithRegistry(regIf),
		vshcore.WithPolicy(pol),
		vshcore.WithBaseEnv(layoutEnv),
		vshcore.WithFileSystem(vshcore.CustomFileSystem(factory, "/")),
		vshcore.WithTracing(vshcore.TraceConfig{Mode: vshcore.TraceRedacted}),
	}
	if cfg.Network != nil {
		opts = append(opts, vshcore.WithNetworkClient(cfg.Network))
	}
	if cfg.BuiltinCommandDir != "" {
		opts = append(opts, vshcore.WithBuiltinCommandDir(cfg.BuiltinCommandDir))
	}
	rt, err := vshcore.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("vsh glue: new runtime: %w", err)
	}
	e.rt = rt
	return e, nil
}

// fallbackRegistry Registry 包装：base 未命中时交兜底（宿主原生适配）；
// 命中（含兜底合成）先过 CommandAllow 执行规则门——拒绝返回权限错误命令，
// 不继续 fallback。Names 只列 base（stub 钉板/命令发现不含动态原生名）。
type fallbackRegistry struct {
	base     *commands.Registry
	fallback func(string) (commands.Command, bool)
	allow    func(ctx context.Context, name string) bool
}

// gated 把命中命令包上执行规则门（每次调用取当次规则——grant/cfg 动态生效）。
func (r fallbackRegistry) gated(name string, c commands.Command) commands.Command {
	if r.allow == nil {
		return c
	}
	allow := r.allow
	return commands.DefineCommand(name, func(ctx context.Context, inv *commands.Invocation) error {
		if !allow(ctx, name) {
			return commands.Exitf(inv, 126, "%s: command denied by exec rules（grant cmd %s 申请）", name, name)
		}
		return c.Run(ctx, inv)
	})
}

func (r fallbackRegistry) Lookup(name string) (commands.Command, bool) {
	if c, ok := r.base.Lookup(name); ok {
		return r.gated(name, c), true
	}
	if r.fallback != nil {
		if c, ok := r.fallback(name); ok {
			return r.gated(name, c), true
		}
	}
	return nil, false
}

func (r fallbackRegistry) Register(cmd commands.Command) error { return r.base.Register(cmd) }

func (r fallbackRegistry) RegisterLazy(name string, loader commands.LazyCommandLoader) error {
	return r.base.RegisterLazy(name, loader)
}

func (r fallbackRegistry) Names() []string { return r.base.Names() }

// Registry 暴露组合 Registry（browser/cua 等端侧指令在此追加注册）。
func (e *Engine) Registry() *commands.Registry { return e.reg }

// ExecRequest 一次脚本执行。
type ExecRequest struct {
	SessionKey string // 会话键（cloud/host = sid）
	// Owner 任务归属的用户身份（cloud="u:"+uid 或 uid；host=host owner uid；
	// 空=匿名共池）。bg 归属 = (Owner, SessionKey)。
	Owner  string
	Script string // 脚本正文
	// WorkDir 空 = 会话默认工作目录（每次 exec 显式给——派生会话不持久 cwd）。
	WorkDir string
	Env     map[string]string // 覆盖 BaseEnv
	// GrantApproved 可信审批事实：服务端已批准「本次脚本可通过 grant 修改
	// 授权」。默认 false；同次执行的 eval/source、管道和超时转后台继承，
	// 后续独立 exec 不继承。
	GrantApproved bool
	// NoSandbox 本次执行的显式免沙箱选项（发送前审批；不派生 grant 权利）。
	NoSandbox bool
	Stdin     io.Reader
	// Timeout 运行期限（≤0 或超上限 = BackgroundWallClock）。
	Timeout time.Duration
	// WaitBudget 调用方前台等待预算（bg wait 共享）；0 = 无（后台执行）。
	WaitBudget time.Duration
	// Stdout/Stderr 可选 stdio 接线（bash 语义）：非空时引擎输出全量透传
	// 进对应 writer（引擎只写——不创建、不命名、不关闭；日志文件由调用方
	// 持有）。返回值（下方 ExecResult 采集串）不受接线影响。
	Stdout io.Writer
	Stderr io.Writer
	// Handle 本次执行的外层句柄（bg wait 禁止等待自身）；空 = 无。
	Handle *ExecHandle
}

// ExecResult 执行结果：stdout/stderr 采集串 + exit_code，仅此而已。
// 预览截断、attrs、日志文件都是调用方（exec 接入层）的职责，不在引擎。
type ExecResult struct {
	ExitCode        int
	Stdout          string
	Stderr          string
	StdoutTruncated bool
	StderrTruncated bool
	Duration        time.Duration
	// Writes 是本次执行的进程内文件写路径审计（trace file.mutation）。
	Writes []string
}

// Exec 执行脚本（panic 隔离：引擎 panic 不带崩 pod 进程）。
// 每次 exec 使用独立派生会话：不持有跨调用会话锁——另一执行（含
// `bg list | grep running` 组合脚本）永不被本执行阻塞。
func (e *Engine) Exec(ctx context.Context, req ExecRequest) (res *ExecResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			res, err = nil, fmt.Errorf("vsh engine panic: %v", r)
		}
	}()
	// 每次 exec 经 NewSessionFS(baseKey) + NewSession 建独立会话：与其他
	// 执行同 backing（UFS/宿主盘状态共享）、独立内存层与工作目录，互不
	// 持锁互等（另一执行/组合脚本永不被本执行阻塞）。
	baseKey := baseSessionKey(req.SessionKey)
	fsys, sessWorkDir, err := e.cfg.NewSessionFS(baseKey)
	if err != nil {
		return nil, fmt.Errorf("vsh glue: session fs: %w", err)
	}
	sess, err := e.rt.NewSession(context.WithValue(ctx, sessionFSKey{}, fsys))
	if err != nil {
		return nil, fmt.Errorf("vsh glue: new session: %w", err)
	}
	// 可信身份与审批事实注入 ctx（不从脚本可修改的 env 取授权信息）。
	ctx = context.WithValue(ctx, netSessionKey{}, baseKey)
	ctx = context.WithValue(ctx, ownerKey{}, req.Owner)
	ctx = context.WithValue(ctx, grantApprovedKey{}, req.GrantApproved)
	ctx = context.WithValue(ctx, noSandboxKey{}, req.NoSandbox)
	if req.WaitBudget > 0 {
		ctx = context.WithValue(ctx, waitBudgetKey{}, time.Now().Add(req.WaitBudget))
	}
	if req.Handle != nil {
		ctx = context.WithValue(ctx, execHandleKey{}, req.Handle)
	}
	timeout := req.Timeout
	if timeout <= 0 || timeout > BackgroundWallClock {
		timeout = BackgroundWallClock
	}
	env := map[string]string{}
	maps.Copy(env, e.cfg.BaseEnv)
	maps.Copy(env, req.Env)
	workDir := req.WorkDir
	if workDir == "" {
		workDir = sessWorkDir
	}
	ereq := &vshcore.ExecutionRequest{
		Script:  req.Script,
		WorkDir: workDir,
		Env:     env,
		Stdin:   req.Stdin,
		Timeout: timeout,
	}
	if req.Stdout != nil {
		ereq.Stdout = req.Stdout
	}
	if req.Stderr != nil {
		ereq.Stderr = req.Stderr
	}
	result, err := sess.Exec(ctx, ereq)
	if err != nil {
		return nil, err
	}
	res = &ExecResult{
		ExitCode:        result.ExitCode,
		Stdout:          result.Stdout,
		Stderr:          result.Stderr,
		StdoutTruncated: result.StdoutTruncated,
		StderrTruncated: result.StderrTruncated,
		Duration:        result.Duration,
	}
	// FS 写审计（进程内文件写路径记录）。
	for _, ev := range result.Events {
		if ev.Kind == trace.EventFileMutation && ev.File != nil {
			res.Writes = append(res.Writes, ev.File.Path)
		}
	}
	return res, nil
}

// baseSessionKey 派生会话（"sid#exec-..."）归一到基键——NewSessionFS 只见
// 基键（backing/规则表按基键路由；派生会话独立内存层）。
func baseSessionKey(key string) string {
	if i := strings.IndexByte(key, '#'); i > 0 {
		return key[:i]
	}
	return key
}

// --- exec 接入层编排已移出（libs/execwait）：vsh 引擎只负责执行 script 并
// 返回 stdout/stderr/exit_code；前台等待、超时转后台、日志与响应 shaping
// 都是调用方职责（§2.4/§2.5，2026-09-28 用户裁定重分层）。 ---

// TaskMeta 是转后台登记的元信息（日志路径 + 请求关联）。
type TaskMeta struct {
	// Owner/Session 归属：list/wait/kill/cancel 按 (user, session) 检查。
	Owner, Session string
	// RequestID 请求关联（cancel 按 request_id 找到同一执行）。
	RequestID string
	// LogOut/LogErr stdout/stderr 日志地址（对应端可读取路径）。
	LogOut, LogErr string
}

// --- 后台任务登记表（bg list/wait/kill；唯一来源 = exec 等待超时 Adopt） ---

// Task 是后台任务的快照。
type Task struct {
	ID         string
	Command    string
	Status     string // running / done / timeout / killed / error
	ExitCode   int
	StartedAt  time.Time
	FinishedAt time.Time
	Err        string
	// Owner/Session 归属（user_id, session_id）；RequestID 请求关联（cancel）。
	Owner, Session string
	RequestID      string
	// LogOut/LogErr 是 stdout/stderr 日志地址（bg list/wait 报告用；
	// 输出读取经 FS/cat，任务表不另存输出缓冲）。
	LogOut, LogErr string
}

// ExecHandle 是一次执行的外层句柄：前台等待、转后台登记与取消共用。
type ExecHandle struct {
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	res    *ExecResult
	err    error
	taskID string
}

func NewExecHandle(cancel context.CancelFunc) *ExecHandle {
	return &ExecHandle{cancel: cancel, done: make(chan struct{})}
}

// Done 完成通知（执行结束关闭）。
func (h *ExecHandle) Done() <-chan struct{} { return h.done }

// DoneFlag 非阻塞完成查询。
func (h *ExecHandle) DoneFlag() bool {
	select {
	case <-h.done:
		return true
	default:
		return false
	}
}

// Finish 写入最终结果并关闭完成通知（只生效一次）。
func (h *ExecHandle) Finish(res *ExecResult, err error) {
	h.mu.Lock()
	select {
	case <-h.done:
		h.mu.Unlock()
		return
	default:
	}
	h.res, h.err = res, err
	close(h.done)
	h.mu.Unlock()
}

// Result 取最终结果（未完成返回 nil, nil）。
func (h *ExecHandle) Result() (*ExecResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.res, h.err
}

// Cancel 终止本次执行（脚本及受管子进程）。与 BindCancel 并发安全：
// cancel(request_id)/DisconnectTools 可能与 execwait 编排层绑定墙钟
// cancel 并发，必须经锁读取（否则会读到半构造的 cancel 或漏取）。
func (h *ExecHandle) Cancel() {
	h.mu.Lock()
	cancel := h.cancel
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// BindCancel 绑定取消函数（接入层编排 execwait 注入墙钟 ctx 的 cancel——
// 外层预建句柄后由编排层接管执行期限）。
func (h *ExecHandle) BindCancel(cancel context.CancelFunc) {
	h.mu.Lock()
	h.cancel = cancel
	h.mu.Unlock()
}

// TaskID 转后台后分配的任务 ID（未登记为空）。
func (h *ExecHandle) TaskID() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.taskID
}

// adopt 登记任务 ID；已完成或未运行返回 false（调用方裁决竞争）。
func (h *ExecHandle) adopt(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	select {
	case <-h.done:
		return false
	default:
	}
	if h.taskID != "" {
		return false
	}
	h.taskID = id
	return true
}

type taskEntry struct {
	mu       sync.Mutex
	task     Task
	handle   *ExecHandle
	finished bool
	seq      int
	// released 容量计数是否已释放（mu 守卫）——只在 finalizeLocked 实际
	// 终结时释放一次（Kill 不再同步释放：容量反映真实在跑数量）。
	released bool
}

// TaskTable 后台任务登记表（exec 超时转后台的唯一来源；不启动执行、不存
// 输出缓冲）。容量闸：全局 max(2, NumCPU/2) + per-owner 4 同时运行上限，
// 超额快速拒绝。沿用完成记录的有界清理，不做持久任务恢复。
type TaskTable struct {
	mu             sync.Mutex
	tasks          map[string]*taskEntry
	seq            int
	running        int
	runningByOwner map[string]int
	maxGlobal      int
	maxPerOwner    int
	maxRetained    int
}

func NewTaskTable() *TaskTable {
	return NewTaskTableWithCaps(maxRunningTasksGlobal(), MaxRunningTasksPerOwner)
}

// NewTaskTableWithCaps 自定义容量闸（测试用；生产走 NewTaskTable）。
func NewTaskTableWithCaps(maxGlobal, maxPerOwner int) *TaskTable {
	return &TaskTable{
		tasks:          map[string]*taskEntry{},
		runningByOwner: map[string]int{},
		maxGlobal:      maxGlobal,
		maxPerOwner:    maxPerOwner,
		maxRetained:    MaxRetainedFinishedTasks,
	}
}

// Adopt 把一次仍在运行的执行登记为后台任务：分配 ID、容量计数；完成监视
// 从 handle 终结状态回填（不重启、不重放、不更换日志）。已完成或已登记的
// 句柄返回错误（exec 外层据此回退为完成结果——竞争只产生一个结果）。
func (t *TaskTable) Adopt(h *ExecHandle, command string, meta TaskMeta) (Task, error) {
	t.mu.Lock()
	// 容量检查前先惰性结算：已完成任务同步释放名额——否则满载时连
	// bg list/kill 都无法执行（任何新 exec 都被拒），形成死锁。
	t.evictFinishedLocked()
	if t.running >= t.maxGlobal {
		n := t.running
		t.mu.Unlock()
		return Task{}, fmt.Errorf("task table full（全局运行上限 %d，当前 %d）——先 bg list 查看、bg kill 释放或稍后重试", t.maxGlobal, n)
	}
	if t.runningByOwner[meta.Owner] >= t.maxPerOwner {
		n := t.runningByOwner[meta.Owner]
		t.mu.Unlock()
		return Task{}, fmt.Errorf("task table full（单归属运行上限 %d，当前 %d）——先 bg list 查看、bg kill 释放或稍后重试", t.maxPerOwner, n)
	}
	t.seq++
	id := fmt.Sprintf("bg-%d", t.seq)
	// 句柄登记（原子裁决完成/超时竞争，t.mu 持有期间调用——h.mu 与 t.mu
	// 无反向锁序）：失败 = 已完成/已登记，回退为完成结果由调用方处理。
	if !h.adopt(id) {
		t.mu.Unlock()
		return Task{}, fmt.Errorf("execution already finished")
	}
	entry := &taskEntry{handle: h, seq: t.seq}
	entry.task = Task{ID: id, Command: command, Status: "running", StartedAt: time.Now(),
		Owner: meta.Owner, Session: meta.Session, RequestID: meta.RequestID,
		LogOut: meta.LogOut, LogErr: meta.LogErr}
	t.tasks[id] = entry
	t.running++
	t.runningByOwner[meta.Owner]++
	t.evictFinishedLocked()
	t.mu.Unlock()
	// 终态惰性回填：读路径（List/Get/Wait/Kill/evict）经 finalizeLocked 从
	// 句柄结果结算——Wait 解除阻塞即终态可见，容量同步恢复（无 watcher
	// goroutine 的异步窗口）。
	return entry.snapshot(), nil
}

// finalizeLocked 从执行句柄结算终态（幂等；t.mu 已持有，锁序
// t.mu→entry.mu→handle.mu）。容量计数在实际终结时同步释放，只发生一次。
func (t *TaskTable) finalizeLocked(e *taskEntry) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.finished || !e.handle.DoneFlag() {
		return
	}
	res, err := e.handle.Result()
	e.task.FinishedAt = time.Now()
	switch {
	case e.task.Status == "killed":
		// Kill 已置位——130 不被执行结果覆盖（kill 时进程可能仍正常退出 0）。
	case err != nil:
		if res != nil {
			e.task.ExitCode = res.ExitCode
		}
		e.task.Status = "error"
		e.task.Err = err.Error()
	case res != nil && res.ExitCode == 124:
		e.task.ExitCode = 124
		e.task.Status = "timeout"
	default:
		if res != nil {
			e.task.ExitCode = res.ExitCode
		}
		e.task.Status = "done"
	}
	e.finished = true
	if !e.released {
		e.released = true
		t.running--
		t.runningByOwner[e.task.Owner]--
	}
}

// evictFinishedLocked 逐出最旧的已完成任务至保留上限（t.mu 已持有；
// 锁序与 List 一致 t.mu→entry.mu）。
func (t *TaskTable) evictFinishedLocked() {
	for {
		var oldest *taskEntry
		finished := 0
		for _, e := range t.tasks {
			t.finalizeLocked(e)
			e.mu.Lock()
			fin := e.finished
			e.mu.Unlock()
			if !fin {
				continue
			}
			finished++
			if oldest == nil || e.seq < oldest.seq {
				oldest = e
			}
		}
		if finished <= t.maxRetained || oldest == nil {
			return
		}
		delete(t.tasks, oldest.task.ID)
	}
}

// owned 归属检查：(owner, session) 二元组一致才可见/可管（跨会话默认隔离；
// 无会话的手动调用属独立 manual 范围——session 空只匹配空）。
func owned(e *taskEntry, owner, session string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.task.Owner == owner && e.task.Session == session
}

// snapshot 先惰性结算终态再取值（t.mu 由调用方持有）。
func (t *TaskTable) snapshot(e *taskEntry) Task {
	t.finalizeLocked(e)
	return e.snapshot()
}

// List 按登记序返回该归属的任务快照。
func (t *TaskTable) List(owner, session string) []Task {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Task, 0, len(t.tasks))
	for _, e := range t.tasks {
		if owned(e, owner, session) {
			out = append(out, t.snapshot(e))
		}
	}
	return out
}

// Get 取单个任务快照（归属不匹配等同不存在）。
func (t *TaskTable) Get(id, owner, session string) (Task, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.tasks[id]
	if !ok || !owned(e, owner, session) {
		return Task{}, false
	}
	return t.snapshot(e), true
}

// FindByRequest 按 request_id 找任务（cancel 与 bg kill 共用取消句柄）。
func (t *TaskTable) FindByRequest(requestID, owner, session string) (Task, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, e := range t.tasks {
		t.finalizeLocked(e)
		e.mu.Lock()
		if e.task.RequestID == requestID && e.task.Owner == owner && e.task.Session == session {
			e.mu.Unlock()
			return e.snapshot(), true
		}
		e.mu.Unlock()
	}
	return Task{}, false
}

// Wait 等待任务结束（d≤0 不等待，立即返回当前快照；归属不匹配等同不存在）。
func (t *TaskTable) Wait(ctx context.Context, id string, d time.Duration, owner, session string) (Task, error) {
	t.mu.Lock()
	e, ok := t.tasks[id]
	if !ok || !owned(e, owner, session) {
		t.mu.Unlock()
		return Task{}, fmt.Errorf("bg: no such task %q", id)
	}
	t.finalizeLocked(e) // 已终结直接出终态，不进等待
	t.mu.Unlock()
	if d > 0 {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-e.handle.Done():
		case <-ctx.Done():
			return t.snapshot(e), ctx.Err()
		case <-timer.C:
			t.mu.Lock()
			defer t.mu.Unlock()
			return t.snapshot(e), nil
		}
		t.mu.Lock()
		defer t.mu.Unlock()
		return t.snapshot(e), nil // 句柄终结：读取即结算终态
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshot(e), nil
}

// Kill 请求终止任务：置 killed/130 并取消同一执行句柄（归属不匹配等同
// 不存在）。终态（finished/FinishedAt）与容量释放不在此结算——执行实际
// 结束时由 finalizeLocked 回填（§2.6：实际结束后才报终态；bg wait 等待
// 句柄 Done，天然等到真正结束）。killed 状态即时可见 = kill 请求已受理。
func (t *TaskTable) Kill(id, owner, session string) error {
	t.mu.Lock()
	e, ok := t.tasks[id]
	if !ok || !owned(e, owner, session) {
		t.mu.Unlock()
		return fmt.Errorf("bg: no such task %q", id)
	}
	t.finalizeLocked(e) // 已终结按终态报错
	e.mu.Lock()
	if e.finished {
		status := e.task.Status
		e.mu.Unlock()
		t.mu.Unlock()
		return fmt.Errorf("bg: task %q already finished (%s)", id, status)
	}
	e.task.Status = "killed"
	e.task.ExitCode = 130
	handle := e.handle
	e.mu.Unlock()
	if handle != nil {
		handle.Cancel()
	}
	t.mu.Unlock()
	return nil
}

func (e *taskEntry) snapshot() Task {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.task
}

// 确保 vbox 引用在包内成立（FSRuleSet/NetRuleSet 源由适配器文件消费）。
var _ = vbox.Capabilities
