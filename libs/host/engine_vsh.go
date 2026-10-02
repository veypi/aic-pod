package host

// engine_vsh.go 是 host 端 vsh 引擎装配（hosts-vsh-redesign §2）：
// exec 动作的 script 经 dispatch 进入，pod 侧引擎执行。
//
// 策略同源（一套策略源、两种执行机制）：
//   - 进程内（内建命令/重定向/管道）→ fs_host 适配器 + fsauth.Snapshot(sid)
//     的 vbox 表门（first-wins 行序：temp→cfg→builtin deny→便利根）；
//   - 原生子进程 → native 兜底合成（OpenLookup）+ 执行期规则检查 →
//     vbox OS 沙箱（per-call 按当次策略生成，fail-closed）；
//   - 网络 → NetClient 对接 netauth.SnapshotVbox（按 ctx 会话键取快照；
//     host 不做私网阻断——LAN 访问是合法场景，AllowPrivate=true）。
//
// 可信上下文：grant_approved/nosandbox/归属全部经引擎注入的可信 ctx 传递，
// 不从脚本可修改的 argv/env 读取；grant 修改入口由引擎内 grant 命令检查
// GrantApprovedFromContext（pod 唯一审批边界）。
//
// 记录在案的设计决策：
//  1. stub 目录 = 进程级 $HOME/.aic/vsh/bin（2026-10-01 自 {session_root}/
//     .vsh-host 挪出并脱离规则门）——布局初始化（stub 写入、HOME MkdirAll）
//     是引擎自身机械 IO，经 vshcore LayoutFS 专用通道走未过门的 OS 文件
//     系统：权限门管 exec/工具，不管 pod 自身读写；会话侧对 stub 目录无
//     任何写通道（篡改面关闭）。registry 优先下同名文件无法 shadow 平台
//     命令，安全性等价。
//  2. 旧 exec_procs 的授权复核（revoke 杀运行中任务）未随 vbox 迁移保留——
//     bg 由引擎任务表统一承接（30min 墙钟到期 124）。

import (
	"context"
	"errors"
	"fmt"
	"io"
	stdfs "io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/execwait"
	tool "github.com/veypi/aic-pod/libs/hosts_tool"
	"github.com/veypi/aic-pod/libs/proto"
	vshglue "github.com/veypi/aic-pod/libs/vsh"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
	"github.com/veypi/vbox"
	gbfs "github.com/veypi/vsh/fs"
)

// vshState host 引擎装配态（Client 持有，惰性一次性构建）。
type vshState struct {
	once   sync.Once
	engine *vshglue.Engine
	native *vshglue.NativeRegistry
	err    error
}

// engine 取或建 host 引擎（Runtime 单例 per pod 进程）。
func (c *Client) engine() (*vshglue.Engine, error) {
	c.vsh.once.Do(func() {
		c.vsh.engine, c.vsh.native, c.vsh.err = c.buildVSHEngine()
	})
	return c.vsh.engine, c.vsh.err
}

// hostCanonical 把 OS 原生路径转为引擎可见规范形（windows = /c/… 类 Linux
// 形，全局统一；posix 恒等）。引擎只看规范形——PATH 按 : 切分不吃盘符、
// 绝对性判定只看 / 前缀，均不感知盘符。
func hostCanonical(p string) string {
	return proto.NormalizeHostPath(filepath.ToSlash(p))
}

// vshStubRoot 进程级 stub/布局根（$HOME/.aic/vsh，决策 1，见文件头）。
// 原生态（os.MkdirAll 直接用）；进引擎前经 hostCanonical 转换。
func (c *Client) vshStubRoot() string {
	if dir, err := cfg.StateDir(); err == nil {
		return filepath.Join(dir, "vsh")
	}
	return filepath.Join(os.TempDir(), "aic-vsh")
}

func (c *Client) buildVSHEngine() (*vshglue.Engine, *vshglue.NativeRegistry, error) {
	stubRoot := c.vshStubRoot()
	stubBin := filepath.Join(stubRoot, "bin")
	layoutHome := filepath.Join(stubRoot, "home")
	if err := os.MkdirAll(stubBin, 0o700); err != nil {
		return nil, nil, fmt.Errorf("vsh host: stub dir: %w", err)
	}
	if err := os.MkdirAll(layoutHome, 0o700); err != nil {
		return nil, nil, fmt.Errorf("vsh host: layout home: %w", err)
	}
	repairStubExecBits(stubBin)
	// 布局 IO 专用通道（决策 1）：未过规则门的 OS 文件系统——引擎布局初始化
	//（stub 写入、HOME MkdirAll）是 pod 自身机械读写，不受会话策略影响。
	layoutFS, err := vshglue.NewHostFS(vshglue.HostFSConfig{
		Backing: OSVFS{},
		Rules:   func() vbox.FSRuleSet { return vbox.FSRuleSet{DefaultWrite: vbox.EffRW} },
	})
	if err != nil {
		return nil, nil, fmt.Errorf("vsh host: layout fs: %w", err)
	}

	native := vshglue.NewNativeRegistry(vshglue.NativeDeps{
		Manager: c.procs,
		Policy:  c.nativePolicy,
		// 引擎 cwd 是规范形（win = /c/…）——原生进程启动需 OS 路径。
		Workdir:      func(invCwd string) string { return proto.HostPathToOS(invCwd) },
		SessionAllow: c.sessionCmdGrant,
	})
	// 种子白名单 = cfg exec_allow（规则数据，不是注册动作）。
	auth := cfg.AuthSnapshot()
	native.Seed(auth.ExecAllow...)
	native.SetPolicy(auth.ExecPolicy == cfg.PolicyOpen, auth.ExecDeny)

	engine, err := vshglue.NewEngine(vshglue.EngineConfig{
		NewSessionFS: func(sid string) (gbfs.FileSystem, string, error) {
			fsys, err := vshglue.NewHostFS(vshglue.HostFSConfig{
				Backing: OSVFS{},
				Rules:   func() vbox.FSRuleSet { return c.policy.Snapshot(sid) },
			})
			if err != nil {
				return nil, "", err
			}
			return fsys, hostCanonical(c.options().WorkDir), nil
		},
		Network: vshglue.NewNetClient(vshglue.NetClientConfig{
			AllowPrivate: true, // host LAN 合法（私网阻断仅 cloud）
			Rules: func(ctx context.Context) vbox.NetRuleSet {
				return c.netPol.SnapshotVbox(vshglue.SessionFromContext(ctx))
			},
		}),
		Platform: vshglue.PlatformDeps{
			Grant:       c.vshGrant,
			GrantStatus: c.vshGrantStatus,
			// ListHosts/SendUser：host 端无主机目录与通知通道（命令存在，
			// 执行给可读报错——零值降级语义）。
			// Skill host = 设备包管理（download 安装 / list 安装记录 / disable·enable
			// 启停 / remove 卸载）。
			Skill: vshglue.SkillDeps{
				Download: c.skillDownload,
				List: func(ctx context.Context, sessionKey string) (string, error) {
					return c.skills.RecordsJSON()
				},
				SetDisabled: func(ctx context.Context, sessionKey, name string, disabled bool) error {
					return c.skills.SetDisabled(name, disabled)
				},
				Remove: func(ctx context.Context, sessionKey, name string) error {
					return c.skills.Uninstall(name)
				},
			},
			// 命令发现展示过滤（§2.2）：host 只展示核心自定义指令与已装包命令。
			Discoverable: func(name string) bool {
				switch name {
				case "commands", "bg", "grant", "skill":
					return true
				}
				return c.skills.IsPackageCommand(name)
			},
		},
		// 虚拟指令执行规则门：skill 与已装 skill 包根命令（browser/cua 等）
		// 按 cfg exec 域检查（exec_policy/exec_deny/exec_allow + 会话 grant）；
		// 内建与平台基础设施指令放行（它们是 shell 本身，旧模型同样不受
		// exec 域约束）。
		CommandAllow: func(ctx context.Context, name string) bool {
			if name == "skill" || c.skills.IsPackageCommand(name) {
				return c.execAllowed(vshglue.SessionFromContext(ctx), name)
			}
			return true
		},
		// Runtime 级布局环境：stub 写入目标 = PATH 目录（布局 IO 经 LayoutFS
		// 专用通道，不过会话规则门——见文件头决策 1）。引擎可见路径一律规范形。
		LayoutEnv: map[string]string{
			"HOME": hostCanonical(layoutHome),
			"PATH": hostCanonical(stubBin),
			"USER": "agent",
		},
		LayoutFS: layoutFS,
		// 内置名（echo/bg/help…）重写后的 stub 解析目录 = host stub bin：vsh
		// 默认 /bin 只适用于有内存层的 cloud；host 无内存层必须显式指向。
		BuiltinCommandDir: hostCanonical(stubBin),
		// 原生不逐个注册：Registry 未命中经 OpenLookup 合成，执行期规则检查。
		NativeFallback: native.OpenLookup,
	})
	if err != nil {
		return nil, nil, err
	}
	// browser（v6 P5）与 cua（v6 P6）都是已装 skill 包（aic-skills 仓），
	// 不再内建注册——包根命令经 skillrun Registry 懒解析进引擎。
	return engine, native, nil
}

// repairStubExecBits 一次性补齐历史 0644 stub 的执行位（适配层 OpenFile 丢
// perm 的残留——布局初始化只补缺失文件、不覆盖既有位，旧 stub 会永远卡在
// 0644 致 PATH/内置名解析跳过）。stub 目录内容全部应为 0755；best-effort。
func repairStubExecBits(stubBin string) {
	_ = filepath.WalkDir(stubBin, func(p string, d stdfs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			_ = os.Chmod(p, 0o755)
		}
		return nil
	})
}

// vshGrantStatus 是 grant status 的执行体：四域姿态 + 规则表 + 会话级
// 临时授权（只读，不要求 grant_approved）。
func (c *Client) vshGrantStatus(ctx context.Context, sessionKey string) (string, error) {
	a := cfg.AuthSnapshot()
	var b strings.Builder
	// exec 域（policy/deny/allow 三键 + 会话级 cmd 授权）
	fmt.Fprintf(&b, "exec_policy: %s", a.ExecPolicy)
	fmt.Fprintf(&b, "\nexec_deny (%d): %s", len(a.ExecDeny), strings.Join(a.ExecDeny, " "))
	fmt.Fprintf(&b, "\nexec_allow (%d): %s", len(a.ExecAllow), strings.Join(a.ExecAllow, " "))
	c.execGrantMu.RLock()
	session := append([]string(nil), c.execGrants[sessionKey]...)
	c.execGrantMu.RUnlock()
	sort.Strings(session)
	fmt.Fprintf(&b, "\nsession cmd grants (%d, 重启失效): %s", len(session), strings.Join(session, " "))
	// fs 域
	fmt.Fprintf(&b, "\n\nfs_policy: %s", a.FsPolicy)
	rows := c.policy.Rules()
	fmt.Fprintf(&b, "\nfs_rules (%d，文件序，后写者优先):", len(rows))
	for i, r := range rows {
		fmt.Fprintf(&b, "\n  %d. %s [%s]", i+1, r.Raw, r.Source)
	}
	fmt.Fprintf(&b, "\nsession fs grants (%d，表头首命中): %s",
		len(c.policy.SessionGrants(sessionKey)), strings.Join(c.policy.SessionGrants(sessionKey), " "))
	// net/ssh 域
	for _, d := range []struct {
		name  string
		pol   string
		rules []string
		list  []string
	}{
		{"net", a.NetPolicy, a.NetRules, c.netPol.List(sessionKey)},
		{"ssh", a.SshPolicy, a.SshRules, c.sshPol.List(sessionKey)},
	} {
		fmt.Fprintf(&b, "\n\n%s_policy: %s", d.name, d.pol)
		fmt.Fprintf(&b, "\n%s_rules (%d):", d.name, len(d.rules))
		for i, r := range d.rules {
			fmt.Fprintf(&b, "\n  %d. %s", i+1, r)
		}
		fmt.Fprintf(&b, "\n%s allow list (%d，含内建与会话): %s", d.name, len(d.list), strings.Join(d.list, " "))
	}
	return b.String(), nil
}

// vshGrant 是引擎内 grant 命令的执行体（grant_approved 检查已在引擎内
// grant 命令完成——pod 唯一审批边界；此处只执行授权动作）：
// fs/net/ssh 域 temp 授权（temp 行插表头、首命中压一切）或 --permanent
// 落盘；cmd 域会话级记入 execGrants（native IsAllowed 经 SessionAllow
// 即时生效），--permanent 追加 exec_allow 落盘。
func (c *Client) vshGrant(ctx context.Context, sessionKey, domain, target string, permanent bool) (string, error) {
	sid := sessionKey
	switch domain {
	case "fs":
		resp := c.grantFS(sid, "", target, permanent)
		if resp.Error != "" {
			return "", fmt.Errorf("%s", resp.Error)
		}
		return resp.Content, nil
	case "net":
		resp := c.grantTarget(sid, "", "net", target, permanent)
		if resp.Error != "" {
			return "", fmt.Errorf("%s", resp.Error)
		}
		return resp.Content, nil
	case "ssh":
		resp := c.grantTarget(sid, "", "ssh", target, permanent)
		if resp.Error != "" {
			return "", fmt.Errorf("%s", resp.Error)
		}
		return resp.Content, nil
	case "cmd":
		name := strings.TrimSpace(target)
		if name == "" || strings.ContainsAny(name, "/\\ \t") {
			return "", fmt.Errorf("grant cmd: invalid command name %q", target)
		}
		if permanent {
			if err := c.persistGrant("exec", name); err != nil {
				return "", fmt.Errorf("grant cmd: persist: %w", err)
			}
			return fmt.Sprintf("granted cmd: %s（scope=permanent，已追加 exec_allow 落盘；注意：授予解释器 = 授予该进程一切能力）", name), nil
		}
		c.execGrantMu.Lock()
		c.execGrants[sid] = append(c.execGrants[sid], name)
		c.execGrantMu.Unlock()
		return fmt.Sprintf("granted cmd: %s（scope=session，重启失效；注意：授予解释器 = 授予该进程一切能力）", name), nil
	default:
		return "", fmt.Errorf("grant: host 支持 fs/net/ssh/cmd 域")
	}
}

// sessionCmdGrant 是 native IsAllowed 的会话级规则 hook（grant cmd 会话授权）。
func (c *Client) sessionCmdGrant(sid, name string) bool {
	c.execGrantMu.RLock()
	defer c.execGrantMu.RUnlock()
	for _, n := range c.execGrants[sid] {
		if n == name {
			return true
		}
	}
	return false
}

// nativePolicy 是 native 包装器的当次策略快照：会话与免沙箱全部经可信
// ctx 取（引擎注入），不读脚本可修改的 env。FSRules 与进程内 FS 门同源
// （fsauth 的 vbox first-wins 表——darwin 沙箱经它逆序输出）；workdir/cmd
// 供工作区元数据保护行（SnapshotForNative）派生。
func (c *Client) nativePolicy(ctx context.Context, workdir, cmd string) vshglue.NativePolicy {
	sid := vshglue.SessionFromContext(ctx)
	deny, allow := c.netPol.Snapshot(sid)
	fsSnap := c.policy.SnapshotForNative(sid, workdir, cmd)
	return vshglue.NativePolicy{
		WriteRoots: c.policy.WriteRootsFor(sid),
		DenyPaths:  c.policy.DenyPatterns(),
		FSRules:    &fsSnap,
		FsOpen:     c.policy.OpenMode(),
		NetOpen:    c.netPol.OpenMode(),
		NetDeny:    deny,
		NetAllow:   allow,
		NoSandbox:  vshglue.NoSandboxFromContext(ctx),
	}
}

// --- exec 动作的统一外层（§2.4 前台等待/超时登记 + §2.5 统一日志） ---

// execScript 执行 script：execwait 编排（后台墙钟运行 + 前台等待；NATS 超时
// Adopt 转后台，RTC 超时返回 deadline_exceeded 不转 bg）。输出契约：NATS
// （AI 消费）content=stdout 前 1000 行预览（截断置 truncated）；RTC 直连
// 全量 content+attrs（不截断、无 truncated 标记）。attrs 恒含
// action/output/error_output，完成时含 exit_code（stderr 预览按需），转
// 后台含 background/id。日志 = .exec/{short}.stdout.log / .stderr.log 双流
// 全量；FS 写审计追加进 stderr 日志（不污染 stdout 契约）；日志创建失败
// 是明确错误（不静默降级）。
func (c *Client) execScript(ctx context.Context, caller tool.Caller, reqID string, p *wire.ExecPayload) (*wire.ExecResult, error) {
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

	env := map[string]string{
		"PATH":   hostCanonical(filepath.Join(c.vshStubRoot(), "bin")),
		"TMPDIR": hostCanonical(os.TempDir()),
	}
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
	h := vshglue.NewExecHandle(nil)
	c.trackExec(reqID, caller.Subject, sid, caller.ConnectionID, h)

	wait := time.Duration(p.WaitMS) * time.Millisecond
	outcome := execwait.Execute(ctx, engine, vshglue.ExecRequest{
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
	}, wait, vshglue.TaskMeta{
		Owner: caller.Subject, Session: sid, RequestID: reqID,
		LogOut: logOut, LogErr: logErr,
		// 转后台（bg）只服务 NATS/AI 通道；RTC 直连等待超时返回
		// ErrWaitElapsed——执行继续、不产生 bg 记录。
	}, !caller.AllowStreams)

	attrs := map[string]string{"action": "exec", "output": logOut, "error_output": logErr}
	if errors.Is(outcome.Err, execwait.ErrWaitElapsed) {
		// RTC 直连等待超时：执行继续（保留取消登记——cancel(request_id)
		// 与 DisconnectTools 仍可终止）；日志在实际结束时关闭。
		go func() {
			<-h.Done()
			auditWrites(errFile, h)
			_ = outFile.Close()
			_ = errFile.Close()
		}()
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
		go func() {
			<-h.Done()
			auditWrites(errFile, h)
			_ = outFile.Close()
			_ = errFile.Close()
		}()
		return &wire.ExecResult{
			Content: fmt.Sprintf("execution backgrounded (id=%s); output: %s", outcome.Task.ID, logOut),
			Attrs:   attrs,
		}, nil
	}
	// 容量不足取消（转后台登记失败，§2.4）：h.Cancel 是异步的，执行
	// goroutine 仍在收尾写日志——与转后台路径一致，日志由实际执行结束
	// 时关闭；错误响应仍携带本次执行已创建的日志地址（Reply 保留
	// Result）。错误码用资源类 overloaded（与连接/页面/排队上限同码）。
	if errors.Is(outcome.Err, execwait.ErrCapacity) {
		go func() {
			<-h.Done()
			auditWrites(errFile, h)
			_ = outFile.Close()
			_ = errFile.Close()
		}()
		err := wire.Fail("overloaded", "exec: "+outcome.Err.Error())
		if outcome.Result == nil {
			return &wire.ExecResult{Attrs: attrs}, err
		}
		return execResultResponse(outcome.Result, attrs, caller.AllowStreams), err
	}
	// 调用方 ctx 结束（传输断连/前台预算到期，§2.6）：断线不是取消——
	// 停止等待但执行继续（取消登记保留），日志在实际执行结束时关闭。
	if errors.Is(outcome.Err, context.Canceled) || errors.Is(outcome.Err, context.DeadlineExceeded) {
		go func() {
			<-h.Done()
			auditWrites(errFile, h)
			_ = outFile.Close()
			_ = errFile.Close()
		}()
		if outcome.Result == nil {
			return &wire.ExecResult{Attrs: attrs}, outcome.Err
		}
		return execResultResponse(outcome.Result, attrs, caller.AllowStreams), outcome.Err
	}
	// 前台完成（执行错误）：写审计并关闭日志。
	auditWrites(errFile, h)
	_ = outFile.Close()
	_ = errFile.Close()
	if outcome.Err != nil {
		if outcome.Result == nil {
			return &wire.ExecResult{Attrs: attrs}, wire.Fail("internal", "exec: "+outcome.Err.Error())
		}
		// 执行完成但引擎层报错：结果与错误一并带出（Reply 保留 Result）。
		res := execResultResponse(outcome.Result, attrs, caller.AllowStreams)
		return res, wire.Fail("internal", "exec: "+outcome.Err.Error())
	}
	res := outcome.Result
	if res == nil {
		return nil, wire.Fail("internal", "exec: no result")
	}
	return execResultResponse(res, attrs, caller.AllowStreams), nil
}

// execResultResponse 构造完成响应（§3.1 统一输出形状）：attrs 恒含
// action/output/error_output/exit_code。输出策略按通道分：
//   - NATS（AI 消费）：有界预览——content=stdout 前 1000 行、attrs.stderr
//     前 100 行；任一截断（行/引擎采集）置 truncated，全量经
//     attrs.output/error_output 日志读取。
//   - RTC 直连（viewer 等非 AI 消费）：全量 content+attrs——不截断、
//     不转后台、没有 truncated 标记；更多数据（完整日志）经 fs 调用读取。
func execResultResponse(res *vshglue.ExecResult, attrs map[string]string, rtcFull bool) *wire.ExecResult {
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
func auditWrites(errLog *os.File, h *vshglue.ExecHandle) {
	res, _ := h.Result()
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
