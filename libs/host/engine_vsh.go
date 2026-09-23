package host

// engine_vsh.go 是 host 端 vsh 引擎装配（design §4.2 + todo 3.1.3）：
// script 经 dispatch execCmd 的 script 分支进入，pod 侧引擎执行。
//
// 策略同源（一套策略源、两种执行机制）：
//   - 进程内（内建命令/重定向/管道）→ fs_host 适配器 + fsauth.Snapshot(sid)
//     的 vbox 表门（first-wins 行序：temp→cfg→builtin deny→便利根）；
//   - 原生子进程 → native 白名单包装器 → exec_procs OS 沙箱（per-call 按当次
//     策略生成，fail-closed）；
//   - 网络 → NetClient 对接 netauth.SnapshotVbox（按 ctx 会话键取快照，
//     M3c per-session 修复；host 不做私网阻断——LAN 访问是合法场景，
//     AllowPrivate=true）。
//
// 记录在案的设计偏差（详见 §4.2/§4.3）：
//  1. stub 目录用进程级 {session_root}/.vsh-host/bin 而非 {sid}/bin——引擎
//     布局初始化（stub 写入、HOME MkdirAll）吃 Runtime 级 BaseEnv（NewSession
//     时无 sid 上下文），per-sid 目录需 fork 补丁，违背零补丁红线；D14
//     registry 优先下同名文件无法 shadow 平台命令，安全性等价。
//  2. exec_procs 的授权复核（revoke 杀运行中任务）在引擎任务表下不保留——
//     bg 由引擎 TaskTable 统一承接（30min 墙钟到期 124）。

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/exec_procs"
	"github.com/veypi/aic-pod/libs/fsauth"
	"github.com/veypi/aic-pod/libs/proto"
	vshglue "github.com/veypi/aic-pod/libs/vsh"
	"github.com/veypi/vbox"
	"github.com/veypi/vsh/commands"
	gbfs "github.com/veypi/vsh/fs"
)

// vshState host 引擎装配态（Client 持有，惰性一次性构建）。
type vshState struct {
	once   sync.Once
	engine *vshglue.Engine
	native *vshglue.NativeRegistry
	err    error
}

// vshEngine 取或建 host 引擎（Runtime 单例 per pod 进程）。
func (c *Client) vshEngine() (*vshglue.Engine, *vshglue.NativeRegistry, error) {
	c.vsh.once.Do(func() {
		c.vsh.engine, c.vsh.native, c.vsh.err = c.buildVSHEngine()
	})
	return c.vsh.engine, c.vsh.native, c.vsh.err
}

// vshStubRoot 进程级 stub/布局根（偏差 1，见文件头）。
func (c *Client) vshStubRoot() string {
	base := c.sessionRoot
	if base == "" {
		if dir, err := cfg.PublicDir(); err == nil {
			base = filepath.Join(dir, "sessions")
		} else {
			base = filepath.Join(os.TempDir(), "aic")
		}
	}
	return filepath.Join(base, ".vsh-host")
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

	native := vshglue.NewNativeRegistry(vshglue.NativeDeps{
		Manager: c.procs,
		Policy:  c.nativePolicy,
	})
	// 种子白名单 = cfg exec_allow（design §4.2；不含 shell/解释器由配置侧
	// 约束——todo 3.6.2 核对）。
	native.Seed(cfg.AuthSnapshot().ExecAllow...)

	engine, err := vshglue.NewEngine(vshglue.EngineConfig{
		NewSessionFS: func(sid string) (gbfs.FileSystem, string, error) {
			fsys, err := vshglue.NewHostFS(vshglue.HostFSConfig{
				Backing: OSVFS{},
				Rules:   func() vbox.FSRuleSet { return c.policy.Snapshot(sid) },
			})
			if err != nil {
				return nil, "", err
			}
			return fsys, c.options().WorkDir, nil
		},
		Network: vshglue.NewNetClient(vshglue.NetClientConfig{
			AllowPrivate: true, // host LAN 合法（2.4.3：私网阻断仅 cloud）
			Rules:        func(ctx context.Context) vbox.NetRuleSet { return c.netPol.SnapshotVbox(vshglue.SessionFromContext(ctx)) },
		}),
		Platform: vshglue.PlatformDeps{
			Grant: c.vshGrant,
			// ListHosts/SendUser：host 端无主机目录与通知通道（命令存在，
			// 执行给可读报错——零值降级语义）。
		},
		// Runtime 级布局环境：stub 写入目标 = PATH 目录（须在规则表可写区，
		// 进程级 stub 根位于会话区便利根之下）。
		LayoutEnv: map[string]string{
			"HOME": layoutHome,
			"PATH": stubBin,
			"USER": "agent",
		},
	})
	if err != nil {
		return nil, nil, err
	}
	// native 白名单注册进引擎 Registry（白名单外命令 = 引擎 127）。
	if err := native.RegisterInto(engine.Registry()); err != nil {
		return nil, nil, fmt.Errorf("vsh host: register native: %w", err)
	}
	return engine, native, nil
}

// vshGrant 是引擎内 grant 命令的执行体（审批已在服务端完成——脚本含字面
// grant → 恒 4 级；此处只执行授权动作）。复用 grant.go 的成熟实现：
// fs/net/ssh 域 temp 授权（temp 行插表头、首命中压一切——DenyHit 拒批已按
// 2.7.4 删除），cmd 域扩充 native 白名单并即时注册。
func (c *Client) vshGrant(ctx context.Context, sessionKey, domain, target string) (string, error) {
	sid := sessionKey
	switch domain {
	case "fs":
		resp := c.grantFS(sid, "", target, false)
		if resp.Error != "" {
			return "", fmt.Errorf("%s", resp.Error)
		}
		return resp.Content, nil
	case "net":
		resp := c.grantTarget(sid, "", "net", target, false)
		if resp.Error != "" {
			return "", fmt.Errorf("%s", resp.Error)
		}
		return resp.Content, nil
	case "ssh":
		// ssh 域授权入 sshPol（执行面 = 免沙箱内置通道，随 ssh 工具重建另接）。
		resp := c.grantTarget(sid, "", "ssh", target, false)
		if resp.Error != "" {
			return "", fmt.Errorf("%s", resp.Error)
		}
		return resp.Content, nil
	case "cmd":
		name := strings.TrimSpace(target)
		if name == "" || strings.ContainsAny(name, "/\\ \t") {
			return "", fmt.Errorf("grant cmd: invalid command name %q", target)
		}
		eng, native, err := c.vshEngine()
		if err != nil {
			return "", err
		}
		native.Allow(name)
		if err := native.Register(eng.Registry(), name); err != nil {
			return "", err
		}
		return fmt.Sprintf("granted cmd: %s（注意：授予解释器 = 授予该进程一切能力）", name), nil
	default:
		return "", fmt.Errorf("grant: host 支持 fs/net/cmd/ssh 域")
	}
}

// fsSandboxRules 把 fs 域有序规则表映射为沙箱行序快照（M3 行序映射输入）：
// 与工具层判定同源（builtin + cfg 拼接序）；darwin 按表序输出，
// 其余平台消费 deny/writeAllow 字段不受影响。
func fsSandboxRules(p *fsauth.Policy) []exec_procs.SandboxRule {
	rows := p.Rules()
	out := make([]exec_procs.SandboxRule, 0, len(rows))
	for _, r := range rows {
		out = append(out, exec_procs.SandboxRule{Effect: r.Effect, Patterns: r.Patterns})
	}
	return out
}

// nativePolicy 是 native 包装器的当次策略快照（sid/level 经 inv.Env 透传）。
func (c *Client) nativePolicy(inv *commands.Invocation) vshglue.NativePolicy {
	sid := inv.Env["AIC_VSH_SESSION"]
	lvl, _ := strconv.Atoi(inv.Env["AIC_VSH_LEVEL"])
	deny, allow := c.netPol.Snapshot(sid)
	return vshglue.NativePolicy{
		Level:        lvl,
		WriteRoots:   c.policy.WriteRootsFor(sid),
		DenyPaths:    c.policy.DenyPatterns(),
		SandboxRules: fsSandboxRules(c.policy),
		FsOpen:       c.policy.OpenMode(),
		NetOpen:      c.netPol.OpenMode(),
		NetDeny:      deny,
		NetAllow:     allow,
		NoSandbox:    inv.Env["AIC_VSH_NOSANDBOX"] == "1",
	}
}

// --- dispatch execCmd 的 script 分支（todo 3.1.3） ---

// execScriptParams 是新 exec 工具的下发载荷（{script, workdir?, timeout?, stdin?, nosandbox?}）。
type execScriptParams struct {
	Script    string `json:"script"`
	Workdir   string `json:"workdir,omitempty"`
	Timeout   int    `json:"timeout,omitempty"` // 等待上限（秒）；任务本身有 30min 独立墙钟
	Stdin     string `json:"stdin,omitempty"`
	NoSandbox bool   `json:"nosandbox,omitempty"`
}

// execScript 执行 script：analyze 预检（grant 恒 4 级纵深）→ 引擎执行 →
// 超时转 bg（任务表独立墙钟，不取消 ctx）。输出契约：Content = stdout 前
// 1000 行；attrs exit_code/background/output（.exec/{short}.log 全量 tee；
// id 仅在 background=true 时作为后台句柄携带）。
func (c *Client) execScript(ctx context.Context, sid string, req *proto.ToolRequest, p execScriptParams) *proto.ToolResponse {
	if strings.TrimSpace(p.Script) == "" {
		return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateError, Error: "exec: script is required"}
	}
	engine, _, err := c.vshEngine()
	if err != nil {
		return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateError, Error: "exec: engine: " + err.Error()}
	}
	// analyze 预检（语法错直接返回；grant 恒 4 级——服务端已审批，此处纵深）。
	analysis := vshglue.Analyze(p.Script, nil)
	if analysis.SyntaxError != "" {
		return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateError,
			Error: "exec: syntax error: " + analysis.SyntaxError}
	}
	if len(analysis.GrantRequests) > 0 && req.GrantedLevel < 4 {
		return reject(req.MsgID, "exec: script contains grant — requires approval (level 4)")
	}

	workdir := p.Workdir
	if workdir == "" {
		workdir = c.options().WorkDir
	}
	if err := c.ensureSessionWorkDir(sid); err != nil {
		return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateError, Error: err.Error()}
	}
	logPath := c.execLogPath(sid, req.MsgID)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateError, Error: "exec: prepare log: " + err.Error()}
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateError, Error: "exec: open log: " + err.Error()}
	}

	env := map[string]string{
		"PATH":   filepath.Join(c.vshStubRoot(), "bin"),
		"TMPDIR": os.TempDir(),
	}
	if home, herr := os.UserHomeDir(); herr == nil {
		env["HOME"] = home
	}
	if p.NoSandbox {
		env["AIC_VSH_NOSANDBOX"] = "1"
	}
	var stdin io.Reader
	if p.Stdin != "" {
		stdin = strings.NewReader(p.Stdin)
	}

	var res *vshglue.ExecResult
	var runErr error
	task := engine.Tasks.Start(p.Script, logPath, func(runCtx context.Context, _ io.Writer) (int, error) {
		res, runErr = engine.Exec(runCtx, vshglue.ExecRequest{
			SessionKey:   sid,
			Script:       p.Script,
			WorkDir:      workdir,
			Env:          env,
			GrantedLevel: req.GrantedLevel,
			Stdin:        stdin,
			Timeout:      vshglue.BackgroundWallClock,
			LongRunning:  true,
			Log:          logFile,
		})
		if runErr != nil {
			return 1, runErr
		}
		return res.ExitCode, nil
	}, nil)

	wait := time.Duration(p.Timeout) * time.Second
	if wait <= 0 || wait > vshglue.MaxForegroundTimeout {
		wait = vshglue.MaxForegroundTimeout
	}
	done, werr := engine.Tasks.Wait(ctx, task.ID, wait)
	if werr != nil {
		_ = logFile.Close()
		return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateError, Error: "exec: " + werr.Error()}
	}
	attrs := map[string]string{"action": "exec", "output": logPath}
	if done.Status == "running" {
		// 超时转 bg：任务继续（独立墙钟），本次返回执行句柄。
		attrs["background"] = "true"
		attrs["id"] = task.ID
		return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateCompleted,
			Content: fmt.Sprintf("execution backgrounded (id=%s); output: %s", task.ID, logPath),
			Attrs:   attrs}
	}
	// FS 写审计随 .exec 日志落行（todo 3.1.5）。
	if res != nil && len(res.Writes) > 0 {
		fmt.Fprintf(logFile, "\n# vsh fs writes (%d):\n", len(res.Writes))
		for _, w := range res.Writes {
			fmt.Fprintf(logFile, "#   %s\n", w)
		}
	}
	_ = logFile.Close()
	if runErr != nil {
		return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateError,
			Error: "exec: " + runErr.Error(), Attrs: attrs}
	}
	content := ""
	exitCode := 1
	if res != nil {
		exitCode = res.ExitCode
		content = firstLines(res.Stdout, 1000)
		if res.Stderr != "" {
			attrs["stderr"] = firstLines(res.Stderr, 100)
		}
		if res.StdoutTruncated || res.StderrTruncated {
			attrs["truncated"] = "true"
		}
	}
	attrs["exit_code"] = strconv.Itoa(exitCode)
	return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateCompleted,
		Content: content, Attrs: attrs}
}

// firstLines 截取前 n 行。
func firstLines(s string, n int) string {
	if n <= 0 || s == "" {
		return s
	}
	lines := strings.SplitAfter(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "")
}
