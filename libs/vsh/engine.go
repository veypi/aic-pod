// Package vsh 是 aic-pod 的 vsh 引擎集成层（glue，cloud/host 共用）。
//
// 职责（design v4.3 §4）：Engine 生命周期（Runtime 单例 + Session 池 +
// limits/墙钟 + panic recover + 后台任务表 + 日志 tee）、FS 适配器
// （fs_ufs.go / fs_host.go，唯一进程内路径权威）、NetClient（netclient.go）、
// 静态分析（analyze.go）、平台命令（cmds.go）、原生命令包装器（native.go）。
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
	"strconv"
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

// limits 定稿（design §6，已拍板）。
const (
	MaxStdoutBytes       = 8 << 20 // 8 MiB
	MaxStderrBytes       = 1 << 20 // 1 MiB
	MaxFileBytes         = 64 << 20
	MaxCommandCount      = 10000
	MaxLoopIterations    = 10000
	MaxGlobOperations    = 100000
	MaxSubstitutionDepth = 50
	// MaxForegroundTimeout 前台 timeout 上限（page 端 180s 由 exec 工具层 cap）。
	MaxForegroundTimeout = 300 * time.Second
	// BackgroundWallClock 后台任务墙钟：到期以 124 终止（设计 §6：10m → 30min）。
	BackgroundWallClock = 30 * time.Minute

	// MaxRunningTasksPerOwner 单 owner 同时运行任务上限（容量闸，2026-09-24
	// 用户拍板）：单任务只有 30min 墙钟不限并发——bg fan-out（一次调用 seq 1..64
	// bg run yes）/多会话并发可占满全部核。超额快速拒绝（排队本身是 DoS 放大器）。
	MaxRunningTasksPerOwner = 4
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
	// host = OS backing）。返回的 workDir 是该会话的默认工作目录。
	NewSessionFS func(sessionKey string) (fsys gbfs.FileSystem, workDir string, err error)
	// Network 注入 NetClient（cloud 唯一网络权威）；nil = 不注册 curl。
	Network vshnet.Client
	// Platform 平台命令依赖（cmds.go）；零值可用（各命令降级为可读报错）。
	Platform PlatformDeps
	// BaseEnv 每次 exec 注入的基础环境（HOME/PATH 钉死由调用方给；
	// 不跨 exec 持久，ExecRequest.Env 覆盖同名键）。
	BaseEnv map[string]string
	// LayoutEnv 覆盖 Runtime 级布局初始化环境（默认 layoutInitEnv：HOME 钉
	// 内存层 /tmp/.vsh-layout-home）。host 端无内存层，必须显式给可写布局
	// HOME 与 PATH（PATH 目录 = stub 写入目标，须在规则表可写区）。
	LayoutEnv map[string]string
	// Logf 可选日志。
	Logf func(format string, args ...any)
}

// Engine 是 vsh Runtime 单例 + 会话池 + 后台任务表。
type Engine struct {
	cfg   EngineConfig
	rt    *vshcore.Runtime
	reg   *commands.Registry
	Tasks *TaskTable

	mu   sync.Mutex
	sess map[string]*engineSession
}

type engineSession struct {
	sess    *vshcore.Session
	workDir string
}

// sessionFSKey 把每会话 FS 经 ctx 传给 Runtime 的 fs.Factory（Runtime 单例，
// Factory.New(ctx) 在 NewSession 时调用——以此把会话身份带进工厂）。
type sessionFSKey struct{}

// netSessionKey 把会话键经 ctx 传给 NetClient（M3c：规则表 per-session 快照，
// cloud 多用户进程不可用进程级并集——跨用户泄漏授权）。
type netSessionKey struct{}

// ownerKey 把任务容量闸归属经 ctx 透传（bg run 派生任务继承同一 owner）。
type ownerKey struct{}

// OwnerFromContext 取 Exec 注入的容量闸归属（无注入 = 空串匿名共池）。
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

// layoutInitEnv 是 Runtime 级 BaseEnv：仅服务于 NewSession 的布局初始化
// （initializeSandboxLayout 用 r.cfg.BaseEnv 做 MkdirAll(HOME)/Chmod(/tmp)/写
// PATH stub）。HOME 钉进内存层（/tmp 前缀）——真实 HOME（cloud=/u/{uid}）
// 由平台每次 exec 经 ExecRequest.Env 注入（executionEnv 覆盖 BaseEnv），
// Runtime 单例因此可跨用户/会话共享（布局初始化不触碰 jail 内真实路径）。
// host 端无内存层：该 HOME 需落在规则表可写处（M3a host 接线时按会话根调整）。
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
	e := &Engine{cfg: cfg, reg: reg, Tasks: NewTaskTable(), sess: map[string]*engineSession{}}
	// bg run 需要回到 Engine.Exec（cmds.go 不反向引用引擎，由此注入）。
	cfg.Platform.Tasks = e.Tasks
	cfg.Platform.RunBG = e.runBG
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
	opts := []vshcore.Option{
		vshcore.WithRegistry(reg),
		vshcore.WithPolicy(pol),
		vshcore.WithBaseEnv(layoutEnv),
		vshcore.WithFileSystem(vshcore.CustomFileSystem(factory, "/")),
		vshcore.WithTracing(vshcore.TraceConfig{Mode: vshcore.TraceRedacted}),
	}
	if cfg.Network != nil {
		opts = append(opts, vshcore.WithNetworkClient(cfg.Network))
	}
	rt, err := vshcore.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("vsh glue: new runtime: %w", err)
	}
	e.rt = rt
	return e, nil
}

// Registry 暴露组合 Registry（host 端 native 白名单在此追加注册）。
func (e *Engine) Registry() *commands.Registry { return e.reg }

// ExecRequest 一次脚本执行。
type ExecRequest struct {
	SessionKey string            // 会话键（cloud=sid；host=sid）
	// Owner 任务容量闸归属（cloud="u:"+uid；host="host"；空=匿名共池）——
	// 经 ctx 透传，bg run 派生任务继承同一 owner。
	Owner      string
	Script     string            // 脚本正文
	WorkDir    string            // 空 = 会话默认工作目录
	Env        map[string]string // 覆盖 BaseEnv
	// GrantedLevel 当次授予等级（host native 子进程沙箱 profile 选择用；
	// 经 env AIC_VSH_LEVEL 透传给 native 包装器）。
	GrantedLevel int
	Stdin        io.Reader
	Timeout      time.Duration // ≤0 = MaxForegroundTimeout；上限见 LongRunning
	// LongRunning 后台语义：Timeout 上限放宽到 BackgroundWallClock（bg 任务
	// 的执行体走 Exec 但需要 30min 墙钟而非前台 300s 钳制）。
	LongRunning bool
	// Log 非空时 stdout/stderr 全量 tee 进该 writer（.exec/{msg_id}.log 约定）。
	Log io.Writer
}

// ExecResult 执行结果（Content = stdout 前 1000 行的截断展示由 exec 工具层做）。
type ExecResult struct {
	ExitCode        int
	Stdout          string
	Stderr          string
	StdoutTruncated bool
	StderrTruncated bool
	Duration        time.Duration
	// Writes 是本次执行的进程内文件写路径审计（trace file.mutation，§5.1）。
	Writes []string
}

// Exec 执行脚本（panic 隔离：引擎 panic 不带崩 pod 进程，验收 7）。
func (e *Engine) Exec(ctx context.Context, req ExecRequest) (res *ExecResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			res, err = nil, fmt.Errorf("vsh engine panic: %v", r)
		}
	}()
	// 管理面快路径：纯 bg 管理命令（list/wait/kill/output）路由派生会话，
	// 不占用基会话执行锁（vshcore Session.Exec 全程持 s.mu）——2026-09-24 实测
	// 漏洞：前台长任务超时转 bg 后仍持基会话锁，救场的 bg kill 排在锁后到不了
	// 执行层，整个会话活锁至 30min 墙钟。Tasks 表引擎级共享，派生会话功能不变。
	sessionKey := req.SessionKey
	if isPureBGMgmtScript(req.Script) {
		sessionKey = fmt.Sprintf("%s#mgmt-%d", baseSessionKey(req.SessionKey), time.Now().UnixNano())
		defer e.DropSession(sessionKey)
	}
	sess, err := e.session(ctx, sessionKey)
	if err != nil {
		return nil, err
	}
	// 会话键注入 ctx：NetClient 规则表按 sid 取快照（M3c per-session 修复——
	// cloud 多用户进程不能用进程级并集；bg 路径经 runBG→Exec 同样注入）。
	ctx = context.WithValue(ctx, netSessionKey{}, req.SessionKey)
	ctx = context.WithValue(ctx, ownerKey{}, req.Owner)
	timeout := req.Timeout
	maxTimeout := MaxForegroundTimeout
	if req.LongRunning {
		maxTimeout = BackgroundWallClock
	}
	if timeout <= 0 || timeout > maxTimeout {
		timeout = maxTimeout
	}
	env := map[string]string{}
	maps.Copy(env, e.cfg.BaseEnv)
	maps.Copy(env, req.Env)
	// 会话键/授予等级经 env 透传给平台命令与 native 包装器。
	env["AIC_VSH_SESSION"] = req.SessionKey
	if lvl := req.GrantedLevel; lvl > 0 {
		env["AIC_VSH_LEVEL"] = strconv.Itoa(lvl)
	}
	workDir := req.WorkDir
	if workDir == "" {
		workDir = sess.workDir
	}
	ereq := &vshcore.ExecutionRequest{
		Script:  req.Script,
		WorkDir: workDir,
		Env:     env,
		Stdin:   req.Stdin,
		Timeout: timeout,
	}
	if req.Log != nil {
		ereq.Stdout = req.Log
		ereq.Stderr = req.Log
	}
	result, err := sess.sess.Exec(ctx, ereq)
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
	// FS 写审计（§5.1：网络有审计，进程内文件写补写路径记录）。
	for _, ev := range result.Events {
		if ev.Kind == trace.EventFileMutation && ev.File != nil {
			res.Writes = append(res.Writes, ev.File.Path)
		}
	}
	return res, nil
}

// session 取或建会话（同 key 多次 Exec 共享沙箱文件系统状态）。
func (e *Engine) session(ctx context.Context, key string) (*engineSession, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if s, ok := e.sess[key]; ok {
		return s, nil
	}
	fsys, workDir, err := e.cfg.NewSessionFS(baseSessionKey(key))
	if err != nil {
		return nil, fmt.Errorf("vsh glue: session fs: %w", err)
	}
	s, err := e.rt.NewSession(context.WithValue(ctx, sessionFSKey{}, fsys))
	if err != nil {
		return nil, fmt.Errorf("vsh glue: new session: %w", err)
	}
	es := &engineSession{sess: s, workDir: workDir}
	e.sess[key] = es
	return es, nil
}

// DropSession 会话结束清理（会话目录回收时调用；幂等）。
func (e *Engine) DropSession(key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.sess, key)
}

// baseSessionKey 派生会话（"sid#bg-..."）归一到基键——NewSessionFS 只见基键
// （backing/规则表按基键路由；派生会话独立内存层）。
func baseSessionKey(key string) string {
	if i := strings.IndexByte(key, '#'); i > 0 {
		return key[:i]
	}
	return key
}

// runBG 是 bg run 的执行体（墙钟由 TaskTable.Start 统一施加，到期 124）。
// bg 在派生会话执行（key 加 "#bg-" 后缀）：与前台同 backing（UFS/宿主盘状态
// 共享）、独立内存层与工作目录（per-exec 内存层语义），且不与前台 Session.Exec
// 串行化互等（同会话 bg 会死锁——前台 wait 等后台、后台排队等前台）。
func (e *Engine) runBG(ctx context.Context, sessionKey, script, workdir, logPath string, log io.Writer) (int, error) {
	bgKey := fmt.Sprintf("%s#bg-%d", sessionKey, time.Now().UnixNano())
	defer e.DropSession(bgKey)
	res, err := e.Exec(ctx, ExecRequest{
		SessionKey:  bgKey,
		Owner:       OwnerFromContext(ctx), // 派生任务继承同一容量闸归属
		Script:      script,
		WorkDir:     workdir,
		Timeout:     BackgroundWallClock,
		LongRunning: true,
		Log:         log,
	})
	if err != nil {
		return 1, err
	}
	if ctx.Err() == context.DeadlineExceeded {
		return 124, nil
	}
	return res.ExitCode, nil
}

// --- 后台任务表（bg list/wait/kill 闭环，验收 6） ---

// Task 是后台任务的快照。
type Task struct {
	ID         string
	Command    string
	LogPath    string
	Status     string // running / done / timeout / killed / error
	ExitCode   int
	StartedAt  time.Time
	FinishedAt time.Time
	Err        string
}

type taskEntry struct {
	mu       sync.Mutex
	task     Task
	cancel   context.CancelFunc
	done     chan struct{}
	output   *boundedBuffer
	finished bool
}

// TaskTable 后台任务表（引擎统一承接 bg，exec_procs 退役后此处是唯一来源）。
// 容量闸（2026-09-24 用户拍板）：全局 max(2, NumCPU/2) + per-owner 4 同时运行
// 上限，超额快速拒绝——单任务 30min 墙钟只限时长，不限并发时 bg fan-out/多会话
// 并发 yes 可占满全部核（排队拒绝采用：排队本身是 DoS 放大器）。
type TaskTable struct {
	mu             sync.Mutex
	tasks          map[string]*taskEntry
	seq            int
	running        int
	runningByOwner map[string]int
	maxGlobal      int
	maxPerOwner    int
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
	}
}

// Start 登记并启动后台任务：run 在带 BackgroundWallClock 的 ctx 中执行，
// 输出落 output 缓冲（bounded）并可 tee 到 logW；墙钟到期 → Status=timeout、
// ExitCode=124。owner 为容量闸归属（空=匿名共池）；超额拒绝并返回错误。
func (t *TaskTable) Start(command, logPath, owner string, run func(ctx context.Context, log io.Writer) (int, error), logW io.Writer) (Task, error) {
	t.mu.Lock()
	if t.running >= t.maxGlobal {
		n := t.running
		t.mu.Unlock()
		return Task{}, fmt.Errorf("task table full（全局运行上限 %d，当前 %d）——先 bg list 查看、bg kill 释放或稍后重试", t.maxGlobal, n)
	}
	if t.runningByOwner[owner] >= t.maxPerOwner {
		n := t.runningByOwner[owner]
		t.mu.Unlock()
		return Task{}, fmt.Errorf("task table full（单归属运行上限 %d，当前 %d）——先 bg list 查看、bg kill 释放或稍后重试", t.maxPerOwner, n)
	}
	t.seq++
	id := fmt.Sprintf("bg-%d", t.seq)
	entry := &taskEntry{done: make(chan struct{}), output: newBoundedBuffer(MaxStdoutBytes)}
	entry.task = Task{ID: id, Command: command, LogPath: logPath, Status: "running", StartedAt: time.Now()}
	// ctx/cancel 同步建立（goroutine 启动前）——Kill 紧随 Start 时 cancel 必已
	// 就位（竞态：cancel 在 goroutine 内赋值时，Start 后微秒级 Kill 会读 nil
	// 跳过 cancel，任务杀不死跑满 30min——TestTaskTableCapacity 实测揪出）。
	ctx, cancel := context.WithTimeout(context.Background(), BackgroundWallClock)
	entry.cancel = cancel
	t.tasks[id] = entry
	t.running++
	t.runningByOwner[owner]++
	t.mu.Unlock()

	go func() {
		// 后台墙钟（§6：30min，到期 124）——任务表统一施加，执行体不再自带。
		defer cancel()
		var w io.Writer = entry.output
		if logW != nil {
			w = io.MultiWriter(entry.output, logW)
		}
		code, err := run(ctx, w)
		entry.mu.Lock()
		entry.task.FinishedAt = time.Now()
		entry.task.ExitCode = code
		switch {
		case ctx.Err() == context.DeadlineExceeded:
			entry.task.Status = "timeout"
			entry.task.ExitCode = 124
		case ctx.Err() == context.Canceled && entry.task.Status == "killed":
			// Kill 已置位
		case err != nil:
			entry.task.Status = "error"
			entry.task.Err = err.Error()
		default:
			entry.task.Status = "done"
		}
		entry.finished = true
		entry.mu.Unlock()
		// 容量计数必须先于 close(done) 释放——Wait 解除阻塞即代表容量已恢复
		// （TestTaskTableCapacity 竞态：defer 在 close 后跑，释放晚一拍）。
		t.mu.Lock()
		t.running--
		t.runningByOwner[owner]--
		t.mu.Unlock()
		close(entry.done)
	}()
	return entry.snapshot(), nil
}

// List 按启动序返回任务快照。
func (t *TaskTable) List() []Task {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Task, 0, len(t.tasks))
	for _, e := range t.tasks {
		out = append(out, e.snapshot())
	}
	return out
}

// Get 取单个任务快照。
func (t *TaskTable) Get(id string) (Task, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.tasks[id]
	if !ok {
		return Task{}, false
	}
	return e.snapshot(), true
}

// Wait 等待任务结束（d≤0 不等待，立即返回当前快照）。
func (t *TaskTable) Wait(ctx context.Context, id string, d time.Duration) (Task, error) {
	t.mu.Lock()
	e, ok := t.tasks[id]
	t.mu.Unlock()
	if !ok {
		return Task{}, fmt.Errorf("bg: no such task %q", id)
	}
	if d > 0 {
		var deadline <-chan time.Time
		timer := time.NewTimer(d)
		defer timer.Stop()
		deadline = timer.C
		select {
		case <-e.done:
		case <-ctx.Done():
			return e.snapshot(), ctx.Err()
		case <-deadline:
			return e.snapshot(), nil
		}
	}
	return e.snapshot(), nil
}

// Kill 终止任务（置 killed 并 cancel；wall clock 由 Start 的 ctx 携带）。
func (t *TaskTable) Kill(id string) error {
	t.mu.Lock()
	e, ok := t.tasks[id]
	t.mu.Unlock()
	if !ok {
		return fmt.Errorf("bg: no such task %q", id)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.finished {
		return fmt.Errorf("bg: task %q already finished (%s)", id, e.task.Status)
	}
	e.task.Status = "killed"
	e.task.ExitCode = 130
	if e.cancel != nil {
		e.cancel()
	}
	return nil
}

// Output 返回任务迄今的捕获输出（bounded）。
func (t *TaskTable) Output(id string) (string, error) {
	t.mu.Lock()
	e, ok := t.tasks[id]
	t.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("bg: no such task %q", id)
	}
	return e.output.String(), nil
}

func (e *taskEntry) snapshot() Task {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.task
}

// boundedBuffer 上限截断的并发安全缓冲（后台输出兜底，防内存膨胀）。
type boundedBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newBoundedBuffer(max int) *boundedBuffer { return &boundedBuffer{max: max} }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if remain := b.max - len(b.buf); remain > 0 {
		if len(p) > remain {
			b.buf = append(b.buf, p[:remain]...)
		} else {
			b.buf = append(b.buf, p...)
		}
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// 确保 vbox 引用在包内成立（FSRuleSet/NetRuleSet 源由适配器文件消费）。
var _ = vbox.Capabilities
