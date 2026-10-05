package host

// Host execution assembles FS/network policy, platform commands and explicit native execution.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/execution"
	"github.com/veypi/aic-pod/libs/mcpx"
	"github.com/veypi/aic-pod/libs/policy"
	"github.com/veypi/aic-pod/libs/proto"
	rtcwire "github.com/veypi/aic-pod/protocol/hosts_rtc"
	wire "github.com/veypi/aic-pod/protocol/tool"
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
	return proto.NormalizeHostPath(filepath.ToSlash(p))
}

func (c *Client) buildVSHEngine(reg *commands.Registry) (*execution.Engine, error) {
	engine, err := execution.NewEngine(execution.EngineConfig{
		Registry: reg,
		NewSessionFS: func(ctx context.Context, sid string) (gbfs.FileSystem, string, error) {
			fsys, err := execution.NewHostFS(execution.HostFSConfig{
				Backing: OSVFS{},
				Rules:   func() vbox.FSRuleSet { return c.policy.Snapshot(sid) },
			})
			if err != nil {
				return nil, "", err
			}
			return fsys, hostCanonical(c.options().WorkDir), nil
		},
		Network: execution.NewNetClient(execution.NetClientConfig{
			AllowPrivate: true, // host LAN 合法（私网阻断仅 cloud）
			Rules: func(ctx context.Context) vbox.NetRuleSet {
				return c.netPol.Snapshot(execution.SessionFromContext(ctx))
			},
		}),
		Platform: execution.PlatformDeps{
			Grant:       c.vshGrant,
			GrantStatus: c.vshGrantStatus,
			SSH:         execution.SSHDeps{SSH: c.managedSSH, SCP: c.managedSCP, SFTP: c.managedSFTP},
			// Host 不提供 ListHosts/SendUser，因此不注册这两个命令。
			MCP:   mcpx.Command(c.mcpSession),
			Skill: execution.SkillDeps{Fetch: c.skillFetch},

			// 命令发现展示过滤（§2.2）：host 只展示核心自定义指令与已装包命令。
			Discoverable: func(name string) bool {
				switch name {
				case "commands", "bg", "grant", "mcp", "skill", "ssh", "scp", "sftp":
					return true
				}
				return false
			},
		},
		// 虚拟指令执行规则门：skill 与已装 skill 包根命令（browser/cua 等）
		// 按 cfg exec 域检查（exec_policy/exec_rules + 会话 grant）；
		// 内建与平台基础设施指令放行（它们是 shell 本身，旧模型同样不受
		// exec 域约束）。
		CommandAllow: func(ctx context.Context, name string) bool {
			if name == "skill" {
				return c.execAllowed(execution.SessionFromContext(ctx), name)
			}
			return true
		},
		NativeExec: c.nativeExec,
	})
	if err != nil {
		return nil, err
	}
	return engine, nil
}

// vshGrantStatus 是 grant status 的执行体：四域姿态 + 规则表 + 会话级
// 临时授权（只读，不要求 grant_approved）。
func (c *Client) vshGrantStatus(ctx context.Context, sessionKey string) (string, error) {
	a := cfg.AuthSnapshot()
	var b strings.Builder
	// exec 域（policy/deny/allow 三键 + 会话级 cmd 授权）
	fmt.Fprintf(&b, "exec_policy: %s", a.ExecPolicy)
	fmt.Fprintf(&b, "\nexec_rules (%d): %s", len(a.ExecRules), strings.Join(a.ExecRules, " "))
	c.execGrantMu.RLock()
	session := append([]string(nil), c.execGrants[sessionKey]...)
	c.execGrantMu.RUnlock()
	sort.Strings(session)
	fmt.Fprintf(&b, "\nsession cmd grants (%d, 重启失效): %s", len(session), strings.Join(session, " "))
	fmt.Fprintf(&b, "\n\nfs_policy: %s", a.FsPolicy)
	fsRows := c.policy.Snapshot(sessionKey).Rules
	fmt.Fprintf(&b, "\nfs_rules (%d, first match wins):", len(fsRows))
	for i, row := range fsRows {
		fmt.Fprintf(&b, "\n  %d. %s:%s [%s]", i+1, row.Effect, row.Pattern, row.Class)
	}
	for _, domain := range []struct {
		name, mode string
		rules      vbox.NetRuleSet
	}{
		{"net", a.NetPolicy, c.netPol.Snapshot(sessionKey)},
		{"ssh", a.SshPolicy, c.sshPol.Snapshot(sessionKey)},
	} {
		fmt.Fprintf(&b, "\n\n%s_policy: %s\n%s_rules (first match wins):", domain.name, domain.mode, domain.name)
		for i, row := range domain.rules.Rules {
			effect := "deny"
			if row.Allow {
				effect = "allow"
			}
			fmt.Fprintf(&b, "\n  %d. %s:%s", i+1, effect, row.HostPort)
		}
	}
	return b.String(), nil
}

// vshGrant 是引擎内 grant 命令的执行体（grant_approved 检查已在引擎内
// grant 命令完成——pod 唯一审批边界；此处只执行授权动作）：
// fs/net/ssh 域 temp 授权（temp 行插表头、首命中压一切）或 --permanent
// 落盘；cmd 域会话级记入 execGrants（native IsAllowed 经 SessionAllow
// 即时生效），--permanent 追加 exec_rules 落盘。
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
		if err := policy.ValidateCommandName(name); err != nil {
			return "", fmt.Errorf("grant cmd: invalid command name %q", target)
		}
		if permanent {
			if err := c.persistGrant("exec", name); err != nil {
				return "", fmt.Errorf("grant cmd: persist: %w", err)
			}
			return fmt.Sprintf("granted cmd: %s（scope=permanent，已置于 exec_rules 表头 落盘；注意：授予解释器 = 授予该进程一切能力）", name), nil
		}
		c.execGrantMu.Lock()
		c.execGrants[sid] = append(c.execGrants[sid], name)
		c.execGrantMu.Unlock()
		return fmt.Sprintf("granted cmd: %s（scope=session，重启失效；注意：授予解释器 = 授予该进程一切能力）", name), nil
	default:
		return "", fmt.Errorf("grant: host 支持 fs/net/ssh/cmd 域")
	}
}

// nativePolicy 组合当次配置与会话授权，供原生与 skill 进程共用。
func (c *Client) nativePolicy(ctx context.Context, workdir, cmd string) vbox.Policy {
	sid := execution.SessionFromContext(ctx)
	return vbox.Policy{FS: c.policy.SnapshotForNative(sid, workdir, cmd), Net: c.netPol.Snapshot(sid)}
}

// --- exec 动作的统一外层（§2.4 前台等待/超时登记 + §2.5 统一日志） ---

// execScript 执行 script：execution 等待编排（后台墙钟运行 + 前台等待；NATS 超时
// Adopt 转后台，RTC 超时返回 deadline_exceeded 不转 bg）。输出契约：NATS
// （AI 消费）content=stdout 前 1000 行预览（截断置 truncated）；RTC 直连
// 成功时全量 content+attrs（无 truncated 标记），完整输出超限或恢复失败
// 明确报错。attrs 恒含
// action/output/error_output，完成时含 exit_code（stderr 预览按需），转
// 后台含 background/id。日志 = .exec/{short}.stdout.log / .stderr.log 双流
// 全量；FS 写审计追加进 stderr 日志（不污染 stdout 契约）；日志创建失败
// 是明确错误（不静默降级）。
func (c *Client) execScript(ctx context.Context, caller wire.Caller, reqID string, p *wire.ExecPayload) (*wire.ExecResult, error) {
	if p == nil || strings.TrimSpace(p.Script) == "" {
		return nil, wire.Fail("invalid_argument", "exec: script is required")
	}
	engine, err := c.engine()
	if err != nil {
		return nil, wire.Fail("internal", "exec: engine: "+err.Error())
	}
	sid := caller.Origin

	workdir := p.Workdir
	if workdir == "" {
		workdir = hostCanonical(c.options().WorkDir)
	} else {
		workdir = proto.NormalizeHostPath(workdir)
	}
	if err := c.ensureSessionWorkDir(sid); err != nil {
		return nil, wire.Fail("internal", err.Error())
	}
	logOut, logErr := c.execLogPaths(sid, reqID)
	if err := os.MkdirAll(filepath.Dir(logOut), 0o700); err != nil {
		return nil, wire.Fail("internal", "exec: prepare log: "+err.Error())
	}
	outFile, err := os.Create(logOut)
	if err != nil {
		return nil, wire.Fail("internal", "exec: open log: "+err.Error())
	}
	errFile, err := os.Create(logErr)
	if err != nil {
		_ = outFile.Close()
		return nil, wire.Fail("internal", "exec: open log: "+err.Error())
	}

	inherited := os.Environ()
	if !p.NoSandbox && !c.options().NoSandbox {
		inherited = vbox.ScrubEnv(inherited)
	}
	env := proto.HostEnvFromOS(inherited)
	env["TMPDIR"] = hostCanonical(os.TempDir())
	if home, herr := os.UserHomeDir(); herr == nil {
		env["HOME"] = hostCanonical(home)
	}
	var stdin io.Reader
	if p.Stdin != "" {
		stdin = strings.NewReader(p.Stdin)
	}

	// 取消登记表：cancel(request_id) 与转后台后的 bg kill 共用此句柄。
	// 归属统一从 caller.Subject 派生（与 cancelExec 的归属检查同源——
	// NATS 信封 Caller 与 RTC 票据 Subject 都是已认证用户身份；不能用
	// 设备属主 c.uid，否则跨用户归属判定与任务表脱节）。
	h := execution.NewExecHandle(nil)
	c.trackExec(reqID, caller.Subject, sid, caller.ConnectionID, h)

	// Written by onDone before h.Finish publishes completion. Incomplete outcomes
	// must not inspect this snapshot or read the still-open execution logs.
	var logs execLogSnapshot
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
		Stdout:        outFile,
		Stderr:        errFile,
		Handle:        h,
		Timeout:       c.options().ExecTimeout,
	}, wait, execution.TaskMeta{
		Owner: caller.Subject, Session: sid, RequestID: reqID,
		LogOut: logOut, LogErr: logErr,
		// 转后台（bg）只服务 NATS/AI 通道；RTC 直连等待超时返回
		// ErrWaitElapsed——执行继续、不产生 bg 记录。
	}, !caller.Direct, func(res *execution.ExecResult) {
		logs = closeExecLogs(outFile, errFile, res)
	})

	attrs := map[string]string{"action": "exec", "output": logOut, "error_output": logErr}
	if errors.Is(outcome.Err, execution.ErrWaitElapsed) {
		// RTC 直连等待超时：执行继续（保留取消登记——cancel(request_id)
		// 与 DisconnectTools 仍可终止）；日志在实际结束时关闭。
		return &wire.ExecResult{
			Content: fmt.Sprintf("execution still running; output: %s（cancel(request_id) 可终止）", logOut),
			Attrs:   attrs,
		}, wire.Fail("deadline_exceeded", "exec: wait elapsed; execution continues (cancel to stop)")
	}
	if outcome.Background {
		// 超时转 bg：任务继续（独立墙钟）；日志文件在执行结束时关闭。
		// 后台执行的取消走任务表 Kill（RequestID 关联）——从取消登记表
		// 摘除，DisconnectTools 只影响前台执行（断线/停止等待不是取消）。
		c.untrackExec(reqID)
		attrs["background"] = "true"
		attrs["id"] = outcome.Task.ID
		return &wire.ExecResult{
			Content: fmt.Sprintf("execution backgrounded (id=%s); output: %s", outcome.Task.ID, logOut),
			Attrs:   attrs,
		}, nil
	}
	// 容量不足取消（转后台登记失败，§2.4）：h.Cancel 是异步的，执行
	// goroutine 仍在收尾写日志——与转后台路径一致，日志由实际执行结束
	// 时关闭；错误响应仍携带本次执行已创建的日志地址（Reply 保留
	// Result）。错误码用资源类 overloaded（与连接/页面/排队上限同码）。
	if errors.Is(outcome.Err, execution.ErrCapacity) {
		err := wire.Fail("overloaded", "exec: "+outcome.Err.Error())
		if outcome.Result == nil {
			return &wire.ExecResult{Attrs: attrs}, err
		}
		return execResultResponse(outcome.Result, attrs, caller.Direct), err
	}
	// 调用方 ctx 结束（传输断连/前台预算到期，§2.6）：断线不是取消——
	// 停止等待但执行继续（取消登记保留），日志在实际执行结束时关闭。
	if errors.Is(outcome.Err, context.Canceled) || errors.Is(outcome.Err, context.DeadlineExceeded) {
		if outcome.Result == nil {
			return &wire.ExecResult{Attrs: attrs}, outcome.Err
		}
		return execResultResponse(outcome.Result, attrs, caller.Direct), outcome.Err
	}
	// 前台完成（审计与日志关闭已由完成回调处理）。
	if outcome.Err != nil {
		if outcome.Result == nil {
			return &wire.ExecResult{Attrs: attrs}, wire.Fail("internal", "exec: "+outcome.Err.Error())
		}
		// 执行完成但引擎层报错：结果与错误一并带出（Reply 保留 Result）。
		res, outputErr := completedExecResultResponse(outcome.Result, attrs, caller.Direct, logs)
		if outputErr != nil {
			return res, outputErr
		}
		return res, wire.Fail("internal", "exec: "+outcome.Err.Error())
	}
	res := outcome.Result
	if res == nil {
		return nil, wire.Fail("internal", "exec: no result")
	}
	return completedExecResultResponse(res, attrs, caller.Direct, logs)
}

// execLogSnapshot records stream lengths before FS audit lines are appended.
// Completion publishes it only after both logs have closed.
type execLogSnapshot struct {
	stdoutBytes int64
	stderrBytes int64
	err         error
}

func closeExecLogs(stdout, stderr *os.File, res *execution.ExecResult) execLogSnapshot {
	var logs execLogSnapshot
	if res != nil {
		for _, stream := range []struct {
			file      *os.File
			truncated bool
			size      *int64
		}{{stdout, res.StdoutTruncated, &logs.stdoutBytes}, {stderr, res.StderrTruncated, &logs.stderrBytes}} {
			if stream.truncated {
				info, err := stream.file.Stat()
				if err != nil {
					logs.err = errors.Join(logs.err, err)
				} else {
					*stream.size = info.Size()
				}
			}
		}
	}
	auditWrites(stderr, res)
	logs.err = errors.Join(logs.err, stdout.Close(), stderr.Close())
	return logs
}

// completedExecResultResponse recovers full RTC streams from this execution's
// closed logs when the engine's bounded in-memory captures were truncated.
// Errors retain log paths, but never advertise a partial JSON payload as success.
func completedExecResultResponse(res *execution.ExecResult, attrs map[string]string, rtcFull bool, logs execLogSnapshot) (*wire.ExecResult, error) {
	if !rtcFull {
		return execResultResponse(res, attrs, false), nil
	}
	attrs["exit_code"] = strconv.Itoa(res.ExitCode)
	fail := func(code, message string) (*wire.ExecResult, error) {
		delete(attrs, "stderr")
		return &wire.ExecResult{Attrs: attrs}, wire.Fail(code, "exec: "+message)
	}
	if logs.err != nil {
		return fail("internal", "finish output logs: "+logs.err.Error())
	}
	stdoutBytes, stderrBytes := int64(len(res.Stdout)), int64(len(res.Stderr))
	if res.StdoutTruncated {
		stdoutBytes = logs.stdoutBytes
	}
	if res.StderrTruncated {
		stderrBytes = logs.stderrBytes
	}
	if stdoutBytes < int64(len(res.Stdout)) || stderrBytes < int64(len(res.Stderr)) {
		return fail("internal", "complete output log is shorter than the captured output")
	}
	if stdoutBytes < 0 || stderrBytes < 0 || stdoutBytes > rtcwire.ToolResponseLimit || stderrBytes > rtcwire.ToolResponseLimit-stdoutBytes {
		return fail("overloaded", fmt.Sprintf("output exceeds %d bytes; full output is available in the execution logs", rtcwire.ToolResponseLimit))
	}
	// Copy the result so restoring output never changes shared engine/task state.
	full := *res
	for _, stream := range []struct {
		path      string
		truncated bool
		size      int64
		value     *string
	}{{attrs["output"], res.StdoutTruncated, stdoutBytes, &full.Stdout}, {attrs["error_output"], res.StderrTruncated, stderrBytes, &full.Stderr}} {
		if !stream.truncated {
			continue
		}
		file, err := os.Open(stream.path)
		if err != nil {
			return fail("internal", "read complete output log: "+err.Error())
		}
		data := make([]byte, int(stream.size))
		_, readErr := io.ReadFull(file, data)
		closeErr := file.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return fail("internal", "read complete output log: "+err.Error())
		}
		*stream.value = string(data)
	}
	full.StdoutTruncated, full.StderrTruncated = false, false
	return execResultResponse(&full, attrs, true), nil
}

// execResultResponse 构造响应（§3.1 统一输出形状）：attrs 恒含
// action/output/error_output/exit_code。输出策略按通道分：
//   - NATS（AI 消费）：有界预览——content=stdout 前 1000 行、attrs.stderr
//     前 100 行；任一截断（行/引擎采集）置 truncated，全量经
//     attrs.output/error_output 日志读取。
//   - RTC 直连（viewer 等非 AI 消费）：成功响应的完整输出须先经
//     completedExecResultResponse 恢复；没有 truncated 标记。
func execResultResponse(res *execution.ExecResult, attrs map[string]string, rtcFull bool) *wire.ExecResult {
	attrs["exit_code"] = strconv.Itoa(res.ExitCode)
	if rtcFull {
		if res.Stderr != "" {
			attrs["stderr"] = res.Stderr
		}
		return &wire.ExecResult{Content: res.Stdout, Attrs: attrs}
	}
	truncated := res.StdoutTruncated || res.StderrTruncated
	content, cut := headLines(res.Stdout, 1000)
	truncated = truncated || cut
	if res.Stderr != "" {
		errHead, errCut := headLines(res.Stderr, 100)
		attrs["stderr"] = errHead
		truncated = truncated || errCut
	}
	if truncated {
		attrs["truncated"] = "true"
	}
	return &wire.ExecResult{Content: content, Attrs: attrs}
}

// auditWrites 把 FS 写审计追加进 stderr 日志（诊断信息——不污染 stdout
// 日志的 --json/管道契约；与执行输出同档持久）。
func auditWrites(errLog *os.File, res *execution.ExecResult) {
	if res == nil || len(res.Writes) == 0 {
		return
	}
	fmt.Fprintf(errLog, "\n# vsh fs writes (%d):\n", len(res.Writes))
	for _, w := range res.Writes {
		fmt.Fprintf(errLog, "#   %s\n", w)
	}
}

// headLines 截取前 n 行；发生截取时返回 truncated=true。
func headLines(s string, n int) (string, bool) {
	if n <= 0 || s == "" {
		return s, false
	}
	lines := strings.SplitAfter(s, "\n")
	if len(lines) <= n {
		return s, false
	}
	return strings.Join(lines[:n], ""), true
}
