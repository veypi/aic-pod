package gitx

// git.go — managed git：把 git 的 ssh 传输层收编进 pod（A-MVP）。
//
// 背景：沙箱内原生 git 的 ssh 传输必然撞三面墙——内建 deny 覆盖 **/.ssh/**、
// env 清洗剥掉 SSH_AUTH_SOCK、net 域需单独授权。旧出路只有 grant fs ~/.ssh
//（rw、会话级、对所有进程可见全部私钥）或 nosandbox（零隔离 + 整单审批），
// 都违背最小权限。
//
// 设计（与 managed ssh 同构）：
//   - Pod 显式注册普通 git 命令。传输类子命令
//    （clone/fetch/pull/push/ls-remote）且 remote 为 ssh 形态时由 pod 侧
//     NoSandbox 执行；其他子命令由同一实现交给 trusted binary + vbox（handled=false，行为与现状
//     完全一致）。
//   - 传输授权复用 ssh 域：URL 解析出 user@host:port 后直接复用 sshOptions
//    （内含 sshAllowed 检查与全套硬化选项）；未授权时报错即 managed ssh 同
//     款「request grant ssh host:port」。grant cmd git 只管本地执行，传输
//     不重复设闸。
//   - pod 侧收敛：受信系统 git（/usr/bin/git——homebrew 前缀用户可写，
//     NoSandbox 下不受信，与 trustedSSHBinary 同原则）、
//     core.hooksPath=/dev/null、选项白名单（-c/--upload-pack/--force 等
//     拒绝）、一次性 GIT_SSH wrapper 把连接参数钉死在已授权目标上——
//     config 里的 insteadOf/core.sshCommand 无法改变实际拨号目标。
//   - https/其他协议、全局选项形态（git -C/-c ...）、Windows 与多 remote
//     传输（--all/--recurse-submodules）本期不拦截，交还原生路径。
//
// 集成测试见 git_integration_test.go（内存 sshd + 真实 git-upload-pack）。
import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/veypi/aic-pod/libs/sshx"
	"github.com/veypi/vbox"
	"github.com/veypi/vsh/commands"
)

// Deps 是 gitx 的宿主依赖。
type Deps struct {
	SSH       *sshx.Runner
	Processes *vbox.Manager
	Run       sshx.ProcessRunner // 测试注入；nil 用 Processes
}

// Runner 是 managed git 的执行器。
type Runner struct{ deps Deps }

func New(deps Deps) *Runner { return &Runner{deps: deps} }

// Managed 是 Pod git 命令的 SSH 传输部分。handled=true 表示本调用已终态
// （含拒绝与用法错误）；false 由调用方在同一实现中走 trusted binary + vbox。
func (r *Runner) Managed(ctx context.Context, inv *commands.Invocation) (bool, error) {
	if runtime.GOOS == "windows" {
		return false, nil // MVP 仅 unix（GIT_SSH wrapper 为 sh 形态）
	}
	call, ok, err := parseGitTransport(inv.Args)
	if !ok {
		return false, nil
	}
	if err != nil {
		return true, commands.Exitf(inv, 2, "git: %v", err)
	}
	cwd := vbox.HostPathToOS(inv.Cwd)
	target, err := gitRemoteForCall(cwd, call)
	if err != nil {
		return true, commands.Exitf(inv, 128, "git: %v", err)
	}
	raw, kind := gitSSHTarget(target.url)
	if kind != gitRemoteSSH {
		return false, nil // https/本地路径等：交还原生（现状不变）
	}
	// ssh.Options 含 ssh 域授权检查（错误文案即 grant 指引）、ssh config
	// 解析（alias/IdentityFile）与全部硬化选项；Exec 是钉死参数的 ssh argv。
	sshOpts, err := r.deps.SSH.Options(ctx, raw, 0)
	if err != nil {
		return true, commands.Exitf(inv, 126, "git: %v", err)
	}
	gitBin, err := TrustedBinary()
	if err != nil {
		return true, commands.Exitf(inv, 126, "git: managed 传输不可用：%v", err)
	}
	dir, err := os.MkdirTemp("", "aic-git-ssh-")
	if err != nil {
		return true, commands.Exitf(inv, 126, "git: %v", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	wrapper := filepath.Join(dir, "git-ssh")
	if err := os.WriteFile(wrapper, []byte(gitSSHWrapperScript(sshOpts.Exec)), 0700); err != nil {
		return true, commands.Exitf(inv, 126, "git: %v", err)
	}
	argv := []string{gitBin, "-c", "core.hooksPath=/dev/null", call.sub}
	argv = append(argv, call.opts...)
	argv = append(argv, target.passArg)
	argv = append(argv, call.rest...)
	env := append([]string{}, sshOpts.Env...)
	env = append(env,
		"GIT_SSH="+wrapper,
		"GIT_SSH_VARIANT=ssh",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_EDITOR=true",
		"GIT_PAGER=cat",
		"PAGER=cat",
	)
	code, err := r.run(ctx, vbox.StartOptions{
		Exec: argv, Env: env, ReplaceEnv: true,
		NoSandbox: true, Workdir: cwd,
	}, inv.Stdin, inv.Stdout, inv.Stderr)
	if err != nil {
		return true, commands.Exitf(inv, 126, "git: %v", err)
	}
	if code != 0 {
		return true, &commands.ExitError{Code: code}
	}
	return true, nil
}

// run 默认走托管进程入口；Deps.Run 供测试注入。
func (r *Runner) run(ctx context.Context, opts vbox.StartOptions, in io.Reader, out, errout io.Writer) (int, error) {
	if r.deps.Run != nil {
		return r.deps.Run(ctx, opts, in, out, errout)
	}
	if r.deps.Processes == nil {
		return 0, fmt.Errorf("git: process manager unavailable")
	}
	return r.deps.Processes.RunProcess(ctx, opts, in, out, errout)
}

// TrustedBinary 受管传输只信任系统拥有的 git（与 trustedSSHBinary 同原则：
// NoSandbox 执行的二进制不得落在用户可写前缀——homebrew 等位置任何登录进程
// 都可替换，会把受管传输变成提权入口）。
func TrustedBinary() (string, error) {
	candidates := []string{"/usr/bin/git", "/bin/git"}
	if runtime.GOOS == "windows" {
		candidates = []string{filepath.Join(os.Getenv("ProgramFiles"), "Git", "cmd", "git.exe")}
	}
	for _, p := range candidates {
		if !filepath.IsAbs(p) {
			continue
		}
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return p, nil
		}
	}
	return "", fmt.Errorf("system-owned git unavailable (expected /usr/bin/git)")
}

// gitSSHWrapperScript 生成一次性 GIT_SSH：丢弃 git 按 URL 推导的 ssh 实参
// （-o/-p/-4/-6 与 [user@]host），把拨号目标钉回授权检查过的 argv。
// 远端命令（git-upload-pack '<path>' 等）经 "$@" 原样透传。
func gitSSHWrapperScript(sshArgv []string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# aic-pod managed git transport (generated, single-use): re-pin every\n")
	b.WriteString("# connection parameter to the grant-checked target; git's own ssh arguments\n")
	b.WriteString("# are dropped so insteadOf/core.sshCommand/URL tricks cannot redirect the dial.\n")
	b.WriteString("while [ \"$#\" -gt 0 ]; do\n  case \"$1\" in\n    -o|-p) shift 2 ;;\n    -4|-6) shift ;;\n    *) break ;;\n  esac\ndone\n")
	b.WriteString("if [ \"$#\" -gt 0 ]; then shift; fi\n")
	b.WriteString("exec")
	for _, a := range sshArgv {
		b.WriteString(" " + shellQuoteArg(a))
	}
	b.WriteString(" \"$@\"\n")
	return b.String()
}

func shellQuoteArg(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			strings.ContainsRune("@%_+=:,./-", r)
		return !ok
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ---- 参数解析（选项白名单，fail-closed） ----

type gitFlagKind uint8

const (
	gitFlagBool     gitFlagKind = iota // 仅裸形（--quiet）
	gitFlagValue                       // 必带值（--depth 1 / --depth=1）
	gitFlagOptValue                    // 可裸可 = 值（--rebase[=merges]）
)

type gitSubSpec struct {
	flags  map[string]gitFlagKind
	reject map[string]string // 显式拒绝（附原因），优先于「不在白名单」报错
	minPos int
	maxPos int // -1 = 不限
	// refspecPlus：位置参数 [1:] 允许 '+' 前缀（fetch/pull 的本地引用强制
	// 更新；push 拒绝——'+' 等价于 --force）。
	refspecPlus bool
}

var gitCommonQuiet = map[string]gitFlagKind{
	"--quiet": gitFlagBool, "-q": gitFlagBool,
	"--verbose": gitFlagBool, "-v": gitFlagBool,
	"--progress": gitFlagBool, "--no-progress": gitFlagBool,
	"--ipv4": gitFlagBool, "-4": gitFlagBool,
	"--ipv6": gitFlagBool, "-6": gitFlagBool,
}

func gitFlags(extra map[string]gitFlagKind) map[string]gitFlagKind {
	out := map[string]gitFlagKind{}
	for k, v := range gitCommonQuiet {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

var gitTransportSpecs = map[string]gitSubSpec{
	"clone": {
		flags: gitFlags(map[string]gitFlagKind{
			"--depth": gitFlagValue, "--branch": gitFlagValue, "-b": gitFlagValue,
			"--origin": gitFlagValue, "-o": gitFlagValue,
			"--jobs": gitFlagValue, "-j": gitFlagValue,
			"--filter": gitFlagValue, "--shallow-since": gitFlagValue,
			"--single-branch": gitFlagBool, "--no-single-branch": gitFlagBool,
			"--bare": gitFlagBool, "--mirror": gitFlagBool,
			"--no-checkout": gitFlagBool, "-n": gitFlagBool, "--no-tags": gitFlagBool,
		}),
		reject: map[string]string{
			"-u":                   "自定义 upload-pack 属于任意命令执行",
			"--upload-pack":        "自定义 upload-pack 属于任意命令执行",
			"-c":                   "内联配置可改写 core.sshCommand 等危险键",
			"--config":             "内联配置可改写 core.sshCommand 等危险键",
			"--template":           "模板目录可携带 hooks",
			"--separate-git-dir":   "MVP 未接入",
			"--recurse-submodules": "子模块传输未接入受管钉扎（host pin 只覆盖主仓库）",
		},
		minPos: 1, maxPos: 2,
	},
	"fetch": {
		flags: gitFlags(map[string]gitFlagKind{
			"--depth": gitFlagValue, "--jobs": gitFlagValue, "-j": gitFlagValue,
			"--prune": gitFlagBool, "-p": gitFlagBool,
			"--tags": gitFlagBool, "-t": gitFlagBool, "--no-tags": gitFlagBool,
			"--force": gitFlagBool, "-f": gitFlagBool, // 本地引用强制更新，远端无损
			"--dry-run": gitFlagBool, "--prune-tags": gitFlagBool, "--unshallow": gitFlagBool,
		}),
		reject: map[string]string{
			"--all":                "多 remote 传输未接入受管钉扎",
			"--multiple":           "多 remote 传输未接入受管钉扎",
			"--recurse-submodules": "子模块传输未接入受管钉扎",
			"--upload-pack":        "自定义 upload-pack 属于任意命令执行",
			"-o":                   "server-option 直送远端 hooks，MVP 未接入",
			"--server-option":      "server-option 直送远端 hooks，MVP 未接入",
			"-c":                   "内联配置可改写 core.sshCommand 等危险键",
			"--config":             "内联配置可改写 core.sshCommand 等危险键",
		},
		minPos: 0, maxPos: -1, refspecPlus: true,
	},
	"pull": {
		flags: gitFlags(map[string]gitFlagKind{
			"--depth":    gitFlagValue,
			"--strategy": gitFlagValue, "-s": gitFlagValue,
			"--strategy-option": gitFlagValue, "-X": gitFlagValue,
			"--rebase": gitFlagOptValue, "--no-rebase": gitFlagBool,
			"--ff-only": gitFlagBool, "--ff": gitFlagBool, "--no-ff": gitFlagBool,
			"--autostash": gitFlagBool, "--no-autostash": gitFlagBool,
			"--squash": gitFlagBool, "--commit": gitFlagBool, "--no-commit": gitFlagBool,
			"--no-edit": gitFlagBool, "--verify": gitFlagBool, "--no-verify": gitFlagBool,
			"--prune": gitFlagBool, "-p": gitFlagBool,
			"--tags": gitFlagBool, "-t": gitFlagBool, "--no-tags": gitFlagBool,
			"--force": gitFlagBool, "-f": gitFlagBool,
		}),
		reject: map[string]string{
			"--upload-pack":        "自定义 upload-pack 属于任意命令执行",
			"--recurse-submodules": "子模块传输未接入受管钉扎",
			"--edit":               "受管传输无交互编辑器",
			"-e":                   "受管传输无交互编辑器",
			"--all":                "多 remote 传输未接入受管钉扎",
			"-c":                   "内联配置可改写 core.sshCommand 等危险键",
			"--config":             "内联配置可改写 core.sshCommand 等危险键",
		},
		minPos: 0, maxPos: -1, refspecPlus: true,
	},
	"push": {
		flags: gitFlags(map[string]gitFlagKind{
			"--set-upstream": gitFlagBool, "-u": gitFlagBool,
			"--delete": gitFlagBool, "--tags": gitFlagBool, "--follow-tags": gitFlagBool,
			"--dry-run": gitFlagBool, "-n": gitFlagBool, "--porcelain": gitFlagBool,
			"--force-with-lease": gitFlagOptValue,
			"--no-verify":        gitFlagBool, "--verify": gitFlagBool,
		}),
		reject: map[string]string{
			"--force":              "破坏性操作不走受管通道（确需强推请用户显式 nosandbox）",
			"-f":                   "破坏性操作不走受管通道（确需强推请用户显式 nosandbox）",
			"--mirror":             "破坏性操作不走受管通道",
			"--all":                "多分支批量推送未接入受管通道",
			"--exec":               "自定义 receive-pack 属于任意命令执行",
			"--receive-pack":       "自定义 receive-pack 属于任意命令执行",
			"-o":                   "push-option 直送远端 hooks，MVP 未接入",
			"--push-option":        "push-option 直送远端 hooks，MVP 未接入",
			"--recurse-submodules": "子模块传输未接入受管钉扎",
			"-c":                   "内联配置可改写 core.sshCommand 等危险键",
			"--config":             "内联配置可改写 core.sshCommand 等危险键",
		},
		minPos: 0, maxPos: -1, refspecPlus: false,
	},
	"ls-remote": {
		flags: gitFlags(map[string]gitFlagKind{
			"--heads": gitFlagBool, "-h": gitFlagBool,
			"--tags": gitFlagBool, "-t": gitFlagBool,
			"--refs": gitFlagBool, "--symref": gitFlagBool,
			"--exit-code": gitFlagBool, "--get-url": gitFlagBool,
			"--sort": gitFlagValue,
		}),
		reject: map[string]string{
			"--upload-pack": "自定义 upload-pack 属于任意命令执行",
			"-c":            "内联配置可改写 core.sshCommand 等危险键",
			"--config":      "内联配置可改写 core.sshCommand 等危险键",
		},
		minPos: 0, maxPos: -1,
	},
}

type gitTransportCall struct {
	sub    string   // clone/fetch/pull/push/ls-remote
	opts   []string // 白名单校验后的选项（原样透传给 pod 侧 git）
	remote string   // 位置参数[0]（clone = URL；其余 = remote 名或 URL），可空
	rest   []string // 其余位置参数（clone 的 dir；fetch/pull/push 的 refspec；ls-remote 的 pattern）
}

// parseGitTransport 判定并解析 git 参数。ok=false：非受管对象（非传输子命令
// 或全局选项形态），交还原生；ok=true 且 err!=nil：形态是受管对象但参数越出
// 白名单——fail-closed 报错，不静默降级到原生路径（那只会以密钥错误收场）。
func parseGitTransport(args []string) (call *gitTransportCall, ok bool, err error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return nil, false, nil
	}
	spec, isTransport := gitTransportSpecs[args[0]]
	if !isTransport {
		return nil, false, nil
	}
	call = &gitTransportCall{sub: args[0]}
	var positional []string
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "--":
			positional = append(positional, rest[i+1:]...)
			i = len(rest)
		case strings.HasPrefix(a, "--"):
			name := a
			hasVal := false
			if j := strings.IndexByte(a, '='); j >= 0 {
				name, hasVal = a[:j], true
			}
			if reason, bad := spec.reject[name]; bad {
				return nil, true, fmt.Errorf("managed git %s: 选项 %s 不受支持（%s）", call.sub, name, reason)
			}
			kind, known := spec.flags[name]
			if !known {
				return nil, true, fmt.Errorf("managed git %s: 选项 %s 不在白名单（复杂操作请拆步，或由用户显式 nosandbox 走原生）", call.sub, name)
			}
			switch kind {
			case gitFlagBool:
				if hasVal {
					return nil, true, fmt.Errorf("managed git %s: 选项 %s 不接受赋值", call.sub, name)
				}
				call.opts = append(call.opts, a)
			case gitFlagOptValue:
				call.opts = append(call.opts, a)
			case gitFlagValue:
				if hasVal {
					call.opts = append(call.opts, a)
				} else {
					if i+1 >= len(rest) {
						return nil, true, fmt.Errorf("managed git %s: 选项 %s 缺少参数值", call.sub, name)
					}
					call.opts = append(call.opts, a, rest[i+1])
					i++
				}
			}
		case strings.HasPrefix(a, "-") && a != "-":
			if reason, bad := spec.reject[a]; bad {
				return nil, true, fmt.Errorf("managed git %s: 选项 %s 不受支持（%s）", call.sub, a, reason)
			}
			kind, known := spec.flags[a]
			if !known {
				return nil, true, fmt.Errorf("managed git %s: 选项 %s 不在白名单（复杂操作请拆步，或由用户显式 nosandbox 走原生）", call.sub, a)
			}
			if kind == gitFlagBool {
				call.opts = append(call.opts, a)
			} else if kind == gitFlagValue {
				if i+1 >= len(rest) {
					return nil, true, fmt.Errorf("managed git %s: 选项 %s 缺少参数值", call.sub, a)
				}
				call.opts = append(call.opts, a, rest[i+1])
				i++
			} else {
				return nil, true, fmt.Errorf("managed git %s: 选项 %s 需要 -- 长形赋值", call.sub, a)
			}
		default:
			positional = append(positional, a)
		}
	}
	if len(positional) < spec.minPos {
		return nil, true, fmt.Errorf("managed git %s: 缺少 remote 参数", call.sub)
	}
	if spec.maxPos >= 0 && len(positional) > spec.maxPos {
		return nil, true, fmt.Errorf("managed git %s: 位置参数过多（%d 个，上限 %d）", call.sub, len(positional), spec.maxPos)
	}
	for i, p := range positional {
		if p == "" || strings.ContainsAny(p, " \t\r\n\x00") {
			return nil, true, fmt.Errorf("managed git %s: 位置参数 %q 非法", call.sub, p)
		}
		if i > 0 && call.sub != "clone" {
			if err := gitRefspecCheck(p, spec.refspecPlus); err != nil {
				return nil, true, fmt.Errorf("managed git %s: 引用参数 %q 非法（%v）", call.sub, p, err)
			}
		}
	}
	if len(positional) > 0 {
		call.remote = positional[0]
		call.rest = positional[1:]
	}
	return call, true, nil
}

func gitRefspecCheck(s string, plus bool) error {
	if strings.HasPrefix(s, "+") {
		if !plus {
			return fmt.Errorf("'+' 前缀等价于 --force")
		}
		s = s[1:]
	}
	if s == "" || strings.HasPrefix(s, "*") || strings.Contains(s, "..") {
		return fmt.Errorf("形态不允许")
	}
	return nil
}

// ---- remote 解析 ----

type gitRemoteKind int

const (
	gitRemoteLocal gitRemoteKind = iota // 本地路径 → 交还原生
	gitRemoteSSH                        // scp-like 或 ssh:// → 受管
	gitRemoteOther                      // https/git/file 等 → 交还原生（MVP）
)

// gitSCPLikeRe 匹配 scp-like 语法 [user@]host:path（git 本体同款判定：
// 第一个斜杠前出现冒号即 ssh）。path 可绝对（/ 开头）。
var gitSCPLikeRe = regexp.MustCompile(`^([A-Za-z0-9_.-]+@)?([A-Za-z0-9_.-]+):(.+)$`)

func classifyGitRemote(ref string) gitRemoteKind {
	if i := strings.Index(ref, "://"); i > 0 {
		if strings.EqualFold(ref[:i], "ssh") {
			return gitRemoteSSH
		}
		return gitRemoteOther
	}
	if strings.HasPrefix(ref, "/") || strings.HasPrefix(ref, "./") ||
		strings.HasPrefix(ref, "../") || strings.HasPrefix(ref, "~") {
		return gitRemoteLocal
	}
	if gitSCPLikeRe.MatchString(ref) {
		return gitRemoteSSH
	}
	return gitRemoteLocal
}

// gitSSHTarget 把 ssh 形态 URL 归一为 managed ssh 的 target 原串
// （[user@]host[:port]，端口随串走，由 resolveSSHTarget 与 ssh config 裁决）。
func gitSSHTarget(ref string) (string, gitRemoteKind) {
	kind := classifyGitRemote(ref)
	if kind != gitRemoteSSH {
		return "", kind
	}
	if strings.Contains(ref, "://") {
		u, err := url.Parse(ref)
		if err != nil || u.Host == "" {
			return "", gitRemoteLocal // 非法 URL 交还原生，让 git 自己报错
		}
		raw := u.Host
		if name := u.User.Username(); name != "" {
			raw = name + "@" + u.Host
		}
		return raw, gitRemoteSSH
	}
	m := gitSCPLikeRe.FindStringSubmatch(ref)
	if m == nil {
		return "", gitRemoteLocal
	}
	return m[1] + m[2], gitRemoteSSH
}

type gitRemoteTarget struct {
	url     string // 授权判定的最终 URL
	passArg string // 传给 pod 侧 git 的 remote 实参（名字或 URL）
}

// gitRemoteForCall 求本次调用的 remote：clone 直接取 URL；其余子命令把
// remote 名（缺省 = 当前分支的 branch.<name>.remote，回退 origin）解析为
// .git/config 里的 url。passArg 恒为显式实参——pod 侧 git 不再依赖分支
// 配置选 remote，保证「检查的 host = 拨号的 host」。
func gitRemoteForCall(repoDir string, call *gitTransportCall) (gitRemoteTarget, error) {
	if call.sub == "clone" {
		return gitRemoteTarget{url: call.remote, passArg: call.remote}, nil
	}
	name := call.remote
	if name == "" {
		name = "origin"
		if branch := gitCurrentBranch(repoDir); branch != "" {
			if data, err := os.ReadFile(filepath.Join(repoDir, ".git", "config")); err == nil {
				if r, ok := gitConfigValue(data, "branch", branch, "remote"); ok && r != "" {
					name = r
				}
			}
		}
	}
	if classifyGitRemote(name) != gitRemoteLocal {
		return gitRemoteTarget{url: name, passArg: name}, nil
	}
	data, err := readGitRepoConfig(repoDir)
	if err != nil {
		return gitRemoteTarget{}, err
	}
	u, ok := gitConfigValue(data, "remote", name, "url")
	if !ok || u == "" {
		return gitRemoteTarget{}, fmt.Errorf("remote %q 没有配置 url", name)
	}
	return gitRemoteTarget{url: u, passArg: name}, nil
}

func readGitRepoConfig(repoDir string) ([]byte, error) {
	dotgit := filepath.Join(repoDir, ".git")
	if st, err := os.Stat(dotgit); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("%s 不是 git 仓库（受管传输需在仓库目录执行；worktree/submodule 的 .git 文件布局 MVP 未接入）", repoDir)
	}
	return os.ReadFile(filepath.Join(dotgit, "config"))
}

func gitCurrentBranch(repoDir string) string {
	data, err := os.ReadFile(filepath.Join(repoDir, ".git", "HEAD"))
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(data))
	if rest, ok := strings.CutPrefix(s, "ref: refs/heads/"); ok {
		return strings.TrimSpace(rest)
	}
	return ""
}

// gitConfigValue 读 .git/config 单值（section 名与 key 不区分大小写，
// subsection 区分）。够用即止：注释、引号值、旧式 [section.sub] 均处理，
// include.path 不展开（MVP）。
func gitConfigValue(data []byte, section, sub, key string) (string, bool) {
	section, key = strings.ToLower(section), strings.ToLower(key)
	curSec, curSub := "", ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			curSec, curSub = gitConfigSection(line)
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if strings.ToLower(strings.TrimSpace(k)) != key || curSec != section || curSub != sub {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
			v = strings.ReplaceAll(v[1:len(v)-1], `\"`, `"`)
		}
		return v, true
	}
	return "", false
}

func gitConfigSection(header string) (string, string) {
	header = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(header, "["), "]"))
	if i := strings.IndexByte(header, '"'); i >= 0 {
		sec := strings.ToLower(strings.TrimSpace(header[:i]))
		sub := strings.Trim(header[i+1:], `" `)
		return sec, sub
	}
	// 旧式 [section.sub]：section 小写、sub 原样（git 兼容路径）。
	if i := strings.IndexByte(header, '.'); i >= 0 {
		return strings.ToLower(header[:i]), header[i+1:]
	}
	return strings.ToLower(header), ""
}
