package host

// Host execution assembles FS/network policy, platform commands and explicit native execution.

import (
	"context"
	"fmt"
	"github.com/veypi/aic-pod/protocol"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/execution"
	"github.com/veypi/aic-pod/libs/gitx"
	"github.com/veypi/aic-pod/libs/sshx"

	"github.com/veypi/vbox"
	"github.com/veypi/vsh/commands"
	gbfs "github.com/veypi/vsh/fs"
)

// vshState host 引擎装配态（Client 持有，初始化时直接装配）。
type vshState struct{ engine *execution.Engine }

func (c *Client) engine() (*execution.Engine, error) {
	if c.initErr != nil {
		return nil, c.initErr
	}
	if c.vsh.engine == nil {
		return nil, fmt.Errorf("execution not initialized")
	}
	return c.vsh.engine, nil
}

// hostCanonical 把 OS 原生路径转为引擎可见规范形（windows = /c/… 类 Linux
// 形，全局统一；posix 恒等）。引擎只看规范形——PATH 按 : 切分不吃盘符、
// 绝对性判定只看 / 前缀，均不感知盘符。
func hostCanonical(p string) string {
	return protocol.NormalizeHostPath(filepath.ToSlash(p))
}

func (c *Client) buildVSHEngine(reg *commands.Registry) (*execution.Engine, error) {
	c.ssh = sshx.New(sshx.Deps{
		Processes: c.procs,
		Run: func(ctx context.Context, opts vbox.StartOptions, in io.Reader, out, errout io.Writer) (int, error) {
			if c.sshRun != nil {
				return c.sshRun(ctx, opts, in, out, errout)
			}
			return c.procs.RunProcess(ctx, opts, in, out, errout)
		},
		Allowed: func(ctx context.Context, hostport string) bool {
			return c.perms != nil && c.perms.sshAllowed(execution.SessionFromContext(ctx), hostport)
		},
		CheckLocal: c.preflightSSHLocal,
	})
	c.git = gitx.New(gitx.Deps{
		SSH:       c.ssh,
		Processes: c.procs,
		Run: func(ctx context.Context, opts vbox.StartOptions, in io.Reader, out, errout io.Writer) (int, error) {
			if c.gitRun != nil {
				return c.gitRun(ctx, opts, in, out, errout)
			}
			return c.procs.RunProcess(ctx, opts, in, out, errout)
		},
	})
	engine, err := execution.NewEngine(execution.EngineConfig{
		Registry: reg,
		BaseEnv:  hostIdentityBaseEnv(),
		NewSessionFS: func(ctx context.Context, sid string) (gbfs.FileSystem, string, error) {
			fsys, err := execution.NewHostFS(execution.HostFSConfig{
				Backing: OSVFS{},
				Rules:   func() vbox.FSRuleSet { return c.perms.fsSnapshot(sid) },
			})
			if err != nil {
				return nil, "", err
			}
			return fsys, hostCanonical(c.options().WorkDir), nil
		},
		Network: execution.NewNetClient(execution.NetClientConfig{
			AllowPrivate: true, // host LAN 合法（私网阻断仅 cloud）
			// UseProxy：设备端尊重 curl -x/会话环境变量（HTTPS_PROXY 等）——
			// 用户自己的网络，代理是合法场景（跨境 git/下载）；cloud 关闭。
			UseProxy: true,
			// MaxResponseBytes 缺省 = 0 = 不限制：设备端下载大文件走流式 -o，
			// 内存占用与文件大小无关（磁盘与沙箱 fs 规则自行兜底）。
			Rules: func(ctx context.Context) vbox.NetRuleSet {
				return c.perms.netSnapshot(execution.SessionFromContext(ctx))
			},
		}),
		Platform: execution.PlatformDeps{
			Grant:       c.vshGrant,
			GrantStatus: c.vshGrantStatus,
			SSH:         execution.SSHDeps{SSH: c.ssh.SSH, SCP: c.ssh.SCP, SFTP: c.ssh.SFTP},
			// cloud 专属命令（sessions/list_hosts/send_user）由 AIC 自己注册。
			// mcp 是服务管理面（list/add/remove/start/stop/restart + 调试直通）；
			// 工具调用走服务自身的 vsh 根命令（vtools 登记，见 services.go）。
			MCP: c.cmdMCP,
			// pull/push：设备↔平台云盘传输（/fs/cloud HTTP 面；skill 命令已随
			// NATS fetch 通道一并退役——包装载改用 pull，docs/skill.md §9.2）。
			Pull: c.cmdPull,
			Push: c.cmdPush,

			// 命令发现展示过滤（§2.2）：host 只展示核心自定义指令、已登记的服务
			// 命令（vtools），外加名字不自明的 contrib 命令（html-to-markdown；
			// jq/awk 属通用命令不列）。
			Discoverable: func(name string) bool {
				switch name {
				case "commands", "bg", "grant", "mcp", "ssh", "scp", "sftp", "pull", "push", "html-to-markdown":
					return true
				}
				return c.serviceRegistered(name)
			},
		},
		NativeExec: c.nativeExec,
	})
	if err != nil {
		return nil, err
	}
	return engine, nil
}

// vshGrantStatus 是 grant status 的执行体：四域姿态 + 规则表 + 会话级
// 临时授权（只读，不要求 grant_approved）。实现在 permissionState。
func (c *Client) vshGrantStatus(ctx context.Context, sessionKey string) (string, error) {
	return c.perms.grantStatus(sessionKey), nil
}

// vshGrant 是引擎内 grant 命令的执行体（grant_approved 检查已在引擎内
// grant 命令完成——pod 唯一审批边界；此处只执行授权动作）：
// fs/net/ssh 域 temp 授权（temp 行插表头、首命中压一切）或 --permanent
// 落盘；cmd 域会话级记入 permissionState（execAllowed 即时生效），--permanent 追加 exec_rules 落盘。
func (c *Client) vshGrant(ctx context.Context, sessionKey, domain, target string, permanent bool) (string, error) {
	sid := sessionKey
	switch domain {
	case "fs":
		return c.grantFS(sid, target, permanent)
	case "net", "ssh":
		return c.grantTarget(sid, domain, target, permanent)
	case "cmd":
		name := strings.TrimSpace(target)
		if reservedSSHNative(name) {
			return "", fmt.Errorf("grant cmd %s: reserved SSH command; use grant ssh host:port", name)
		}
		if err := cfg.ValidateCommandName(name); err != nil {
			return "", fmt.Errorf("grant cmd: invalid command name %q", target)
		}
		if permanent {
			if err := c.persistGrant("exec", name); err != nil {
				return "", fmt.Errorf("grant cmd: persist: %w", err)
			}
			return fmt.Sprintf("granted cmd: %s（scope=permanent，已置于 exec_rules 表头 落盘；注意：授予解释器 = 授予该进程一切能力）", name), nil
		}
		c.perms.grantCmd(sid, name)
		return fmt.Sprintf("granted cmd: %s（scope=session，重启失效；注意：授予解释器 = 授予该进程一切能力）", name), nil
	default:
		return "", fmt.Errorf("grant: host 支持 fs/net/ssh/cmd 域")
	}
}

// nativePolicy 组合当次配置与会话授权，供原生与 skill 进程共用。net 域用
// 原生快照（剔除 tool-only 行——它们仅工具层执行，内核不落地）。
func (c *Client) nativePolicy(ctx context.Context, workdir, cmd string) vbox.Policy {
	sid := execution.SessionFromContext(ctx)
	return vbox.Policy{FS: c.perms.fsSnapshotForNative(sid, workdir, cmd), Net: c.perms.netSnapshotForNative(sid)}
}

// --- exec 动作的统一外层（§2.4 前台等待/超时登记 + §2.5 统一日志） ---

// execScript 执行 script：execution 等待编排（后台墙钟运行 + 前台等待；NATS 超时
// Adopt 转后台，RTC 超时返回 deadline_exceeded 不转 bg）。输出契约：NATS
// （AI 消费）content=stdout 前 1000 行预览（截断置 truncated）；RTC 直连
// 成功时全量 content+attrs（无 truncated 标记），完整输出超限或恢复失败
// 明确报错。attrs 恒含 action，完成时含 exit_code（stderr 预览按需），转
// 后台含 background/id；output/error_output 仅在日志文件存在时出现。
// 日志 = .exec/{short}.stdout.log / .stderr.log 双流全量、惰性落盘——
// 仅截断（行预览/引擎采集超限）或执行晚于响应（转后台/等待超时/断连保
// 留/容量取消）才创建文件：有路径 ⟺ 有更多内容；同步完成且未截断的执
// 行不产生文件。日志创建失败是明确错误（不静默降级）。
func (c *Client) execScript(ctx context.Context, caller protocol.Caller, reqID string, p *protocol.ExecPayload) (*protocol.Output, error) {
	if p == nil || strings.TrimSpace(p.Script) == "" {
		return nil, protocol.Fail("invalid_argument", "exec: script is required")
	}
	engine, err := c.engine()
	if err != nil {
		return nil, protocol.Fail("internal", "exec: engine: "+err.Error())
	}
	sid := caller.Origin

	workdir := p.Workdir
	if workdir == "" {
		workdir = hostCanonical(c.options().WorkDir)
	} else {
		workdir = protocol.NormalizeHostPath(workdir)
	}
	if err := c.ensureSessionWorkDir(sid); err != nil {
		return nil, protocol.Fail("internal", err.Error())
	}
	// 惰性落盘（§2.5）：构造不触碰文件系统，溢出/强制时才建文件。
	logOut, logErr := c.execLogPaths(sid, reqID)
	logs := execution.NewLogPair(logOut, logErr)

	inherited := os.Environ()
	if !p.NoSandbox && !c.options().NoSandbox {
		inherited = vbox.ScrubEnv(inherited)
	}
	env := HostEnvFromOS(inherited)
	env["TMPDIR"] = hostCanonical(os.TempDir())
	if home, herr := os.UserHomeDir(); herr == nil {
		env["HOME"] = hostCanonical(home)
	}
	var stdin io.Reader
	if p.Stdin != "" {
		stdin = strings.NewReader(p.Stdin)
	}

	// 唯一运行记录：execution 任务表（cancel 已绑定，登记先于启动）——
	// cancel(request_id)/bg kill/断连都查同一记录，host 不再登记执行。
	// 归属统一从 caller.Subject 派生（与 cancelExec 的归属检查同源——
	// NATS 信封 Caller 与 RTC 票据 Subject 都是已认证用户身份；不能用
	// 设备属主 c.uid，否则跨用户归属判定与任务表脱节）。
	var snapshot execution.LogSnapshot
	wait := time.Duration(p.WaitMS) * time.Millisecond
	outcome := execution.Execute(ctx, engine, execution.ExecRequest{
		SessionKey:    sid,
		Owner:         caller.Subject,
		Script:        p.Script,
		WorkDir:       workdir,
		Env:           env,
		GrantApproved: caller.GrantApproved,
		NoSandbox:     p.NoSandbox,
		Stdin:         stdin,
		Stdout:        logs.Stdout,
		Stderr:        logs.Stderr,
		Timeout:       c.options().ExecTimeout,
	}, wait, execution.TaskMeta{
		Owner: caller.Subject, Session: sid, RequestID: reqID, ConnectionID: caller.ConnectionID,
		LogOut: logOut, LogErr: logErr,
		// 转后台（bg）只服务 NATS/AI 通道；RTC 直连等待超时返回
		// ErrWaitElapsed——执行继续、不产生 bg 记录。
	}, !caller.Direct, func(res *execution.ExecResult) {
		snapshot = logs.CloseResult(res)
	})

	attrs := map[string]string{"action": "exec"}
	// spillLogs 在执行晚于响应返回的分支强制落盘并挂出日志地址（响应发出
	// 后执行仍可能续写——路径必须先存在且持续可写）。
	spillLogs := func() {
		_ = logs.SpillAll()
		logs.AttachPaths(attrs)
	}
	switch outcome.Kind {
	case execution.OutcomeWaitElapsed:
		// RTC 直连等待超时：执行继续（前台记录保留——cancel(request_id)
		// 与 DisconnectTools 仍可终止）；日志在实际结束时关闭。
		spillLogs()
		return &protocol.Output{
			Content: fmt.Sprintf("execution still running; output: %s（cancel(request_id) 可终止）", logOut),
			Attrs:   attrs,
		}, protocol.Fail("deadline_exceeded", "exec: wait elapsed; execution continues (cancel to stop)")
	case execution.OutcomeBackground:
		// 超时转 bg：同一记录标记 bg（任务继续，独立墙钟）；日志文件在
		// 执行结束时关闭。后台执行的取消走任务表 Kill/CancelByRequest，
		// DisconnectTools 只取消前台执行（断线/停止等待不是取消）。
		spillLogs()
		attrs["background"] = "true"
		attrs["id"] = outcome.Task.ID
		return &protocol.Output{
			Content: fmt.Sprintf("execution backgrounded (id=%s); output: %s", outcome.Task.ID, logOut),
			Attrs:   attrs,
		}, nil
	case execution.OutcomeCapacity:
		// 容量不足取消（转后台登记失败，§2.4）：Cancel 是异步的，执行
		// goroutine 仍在收尾写日志——与转后台路径一致，日志由实际执行结束
		// 时关闭；错误响应仍携带本次执行已创建的日志地址（Reply 保留
		// Result）。错误码用资源类 overloaded（与连接/页面/排队上限同码）。
		spillLogs()
		err := protocol.Fail("overloaded", "exec: "+outcome.Err.Error())
		if outcome.Result == nil {
			return &protocol.Output{Attrs: attrs}, err
		}
		return logs.Captured(outcome.Result, attrs, caller.Direct), err
	case execution.OutcomeInterrupted:
		// 调用方 ctx 结束（传输断连/前台预算到期，§2.6）：断线不是取消——
		// 停止等待但执行继续（前台记录保留），日志在实际执行结束时关闭。
		spillLogs()
		if outcome.Result == nil {
			return &protocol.Output{Attrs: attrs}, outcome.Err
		}
		return logs.Captured(outcome.Result, attrs, caller.Direct), outcome.Err
	}
	// OutcomeCompleted：前台完成（日志关闭已由完成回调处理）。执行已终结，
	// 部分产出只活在日志里——错误响应保留其地址（Reply 保留 Result）。
	if outcome.Err != nil {
		if outcome.Result == nil {
			spillLogs()
			return &protocol.Output{Attrs: attrs}, protocol.Fail("internal", "exec: "+outcome.Err.Error())
		}
		if err := logs.Settle(attrs, outcome.Result, !caller.Direct); err != nil {
			return &protocol.Output{Attrs: attrs}, err
		}
		res, outputErr := logs.Completed(outcome.Result, attrs, caller.Direct, snapshot)
		if outputErr != nil {
			return res, outputErr
		}
		return res, protocol.Fail("internal", "exec: "+outcome.Err.Error())
	}
	res := outcome.Result
	if res == nil {
		return nil, protocol.Fail("internal", "exec: no result")
	}
	if err := logs.Settle(attrs, res, !caller.Direct); err != nil {
		return &protocol.Output{Attrs: attrs}, err
	}
	return logs.Completed(res, attrs, caller.Direct, snapshot)
}
