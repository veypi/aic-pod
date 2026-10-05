// Package execution 是 aic-pod 的 vsh 引擎集成层（glue，cloud/host 共用）。
//
// 职责：Engine 生命周期（Runtime 单例 + per-exec 派生会话 + limits/墙钟 +
// panic recover + 后台任务登记表）、FS 适配器（fs_ufs.go / fs_host.go，唯一
// 进程内路径权威）、NetClient（netclient.go）、静态分析（analyze.go）、
// 平台命令（cmds.go）、显式 NativeExec 注入。
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
package execution

import (
	"context"
	"fmt"
	"io"
	"maps"
	"runtime"
	"strings"
	"time"

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
	Registry *commands.Registry
	// NewSessionFS 构建每会话文件系统（cloud = UFS 直通 + 内存层 + jail；
	// host = OS backing）。入参为基会话键（派生后缀已剥离）；返回的 workDir
	// 是该会话的默认工作目录。
	NewSessionFS func(ctx context.Context, sessionKey string) (fsys gbfs.FileSystem, workDir string, err error)
	// Network 注入 NetClient（cloud 唯一网络权威）；nil = 不注册 curl。
	Network vshnet.Client
	// Platform 平台命令依赖（cmds.go）；只注册提供对应能力的命令。
	Platform PlatformDeps
	// BaseEnv 每次 exec 注入的基础环境（HOME/PATH 钉死由调用方给；
	// 不跨 exec 持久，ExecRequest.Env 覆盖同名键）。
	BaseEnv    map[string]string
	NativeExec func(context.Context, string, *commands.Invocation) error
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

func NewEngine(cfg EngineConfig) (*Engine, error) {
	if cfg.NewSessionFS == nil {
		return nil, fmt.Errorf("vsh glue: NewSessionFS required")
	}
	reg := cfg.Registry
	if reg == nil {
		var err error
		reg, err = NewRegistry()
		if err != nil {
			return nil, err
		}
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
	opts := []vshcore.Option{
		vshcore.WithRegistry(reg),
		vshcore.WithPolicy(pol),
		vshcore.WithBaseEnv(cfg.BaseEnv),
		vshcore.WithNativeExec(cfg.NativeExec),
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
	// Timeout 运行期限（≤0 = BackgroundWallClock）。
	Timeout time.Duration
	// WaitBudget 调用方前台等待预算（bg wait 共享）；0 = 无（后台执行）。
	WaitBudget time.Duration
	// Stdout/Stderr 可选 stdio 接线（bash 语义）：非空时引擎输出全量透传
	// 进对应 writer（引擎只写——不创建、不命名、不关闭；日志文件由调用方
	// 持有）。返回值（下方 ExecResult 采集串）不受接线影响。
	Stdout io.Writer
	Stderr io.Writer
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
	fsys, sessWorkDir, err := e.cfg.NewSessionFS(ctx, baseKey)
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
	timeout := req.Timeout
	if timeout <= 0 {
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

// NewRegistry assembles the supported core command set before platform and skill commands.
func NewRegistry() (*commands.Registry, error) {
	reg := vshcore.DefaultRegistry()
	if err := jq.Register(reg); err != nil {
		return nil, err
	}
	return reg, nil
}
