// 进程沙箱（§5.10）：exec 外部进程统一经沙箱包装执行。
//
// 规则：未显式免沙箱（StartOptions.NoSandbox）的进程调用一律进沙箱——
// 审批通过（LevelApproved 9）也不例外：9 只是等级语义，免沙箱唯一通道是
// 显式 nosandbox（外部请求须经人工审批，required Critical(4) ⇒ 必审批）。
// level 1 = read-only（除 /dev/null 外不可写）；level 2/3/4/9 = workspace-write
// （仅工作区 + 常见工具链缓存目录 cacheRoots + 平台临时区可写）；
// 0 = 未设置/异常值，按 read-only 兜底。
// 无可用后端时 fail-closed：拒绝执行，绝不静默裸跑。
//
// 内部指令（fs/curl/json 等）走 vcore VFS + Roots/ProtectRoots 路径收容，
// 不经过本包（文件效应由路径级权限控制）。
//
// 平台后端（planConfined/probeBackend 按构建平台分组实现）：
//   - linux: bubblewrap（bwrap，需用户命名空间可用），无则 fail-closed；
//   - darwin: sandbox-exec（Seatbelt，系统自带，deprecated 但仍在）；
//   - windows: 受限令牌（CreateRestrictedToken）+ ACL 写授权（路径 A：
//     host 进程内创建令牌，SysProcAttr.Token 注入，无独立 runner）；
//   - 其他: 无后端，fail-closed（confined 模式拒绝执行）。
package exec_procs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/veypi/aic-pod/libs/proto"
)

// probeTimeout 是每个后端功能性 probe 的超时（真跑一次最小命令验证；
// 0 会被视为无超时，故必须为正数）。
const probeTimeout = 5 * time.Second

// protectedMetadataNames 是工作区可写时仍保持只读的敏感子路径名
// （借鉴 codex：.git 防 AI 破坏仓库元数据/历史；可扩展 .agents 等）。
// 沙箱内 git 写操作（add/commit）会失败——AI 可经审批 9 免沙箱执行。
// linux 有效性实测（2026-08-14，bwrap / Debian 13）：沙箱内嵌套
// `unshare -rm` 后 umount ro-bind 覆盖、re-bind 工作区两种逃逸均被内核
// 挂载归属规则拒绝（挂载属于父 userns，嵌套 ns 内无 CAP_SYS_ADMIN 可操作）
// ——ro-bind 覆盖是有效边界，非纸面加固。
var protectedMetadataNames = []string{".git"}

// sandboxBackend 是选中的平台后端。
type sandboxBackend int

const (
	backendUnavailable sandboxBackend = iota
	backendBwrap
	backendSeatbelt
	backendWindowsAcl
)

// launchPlan 是一次 confined 启动的平台方案：
//   - argv：替换启动参数（bwrap/seatbelt 包装；windows 原样）
//   - token：windows 受限令牌句柄（其他平台恒 0；spawn 成功后由
//     exec_procs 关闭——子进程持有令牌副本）
//   - env：附加环境变量（windows：TMP/TEMP 指向私有临时目录）
//   - cleanup：进程结束后调用（windows：撤销私有临时目录 ACE 并删除；
//     其他平台 nil）
type launchPlan struct {
	argv    []string
	token   uintptr
	env     []string
	cleanup func()
}

var (
	sandboxMu      sync.Mutex
	sandboxVerdict sandboxBackend = -1 // -1 = 未探测
)

// Confine 将 argv 包装为沙箱执行形态（返回替换 argv；windows 的实际
// confined 路径走 planConfined 的令牌注入，本函数仅供非 windows 调用与
// 统一测试）。
// level 为本次调用的授予等级（仅选择沙箱 profile）：1 = read-only；
// 2/3/4/9 = workspace-write；0 = 未设置/异常值，按 read-only 处理（fail-closed）。
// 审批通过（9）不豁免沙箱——免沙箱不经本函数表达（StartOptions.NoSandbox）。
// 无可用后端返回错误（fail-closed），绝不返回未包装 argv。
func Confine(level int, workdir string, argv []string) ([]string, error) {
	plan, err := planConfined(level, workdir, argv)
	if err != nil {
		return nil, err
	}
	return plan.argv, nil
}

// selectBackend 返回本平台后端（功能性探测一次并缓存整个进程生命周期）。
func selectBackend() sandboxBackend {
	sandboxMu.Lock()
	defer sandboxMu.Unlock()
	if sandboxVerdict < 0 {
		sandboxVerdict = probeBackend()
	}
	return sandboxVerdict
}

// sandboxUnavailable 构造 fail-closed 错误（命令未执行）。
func sandboxUnavailable(level int) error {
	return fmt.Errorf(
		"sandbox: level %d requires confinement but no sandbox backend is usable on this host "+
			"(install bubblewrap on Linux); the command was NOT run — approve level 9 to run unconfined", level)
}

// ---- linux: bubblewrap（跨平台编译的纯 argv 构建，测试直接引用）----

// bwrapArgs 构建 bwrap 包装 argv：
//   - 基础：整机只读挂载（--ro-bind / /）+ /dev + /proc + --die-with-parent
//     （host 退出沙箱进程组随之终止，与 killEntry 进程组语义一致）
//   - workspace-write（level 2/3/4）：/tmp 换 tmpfs（全新空目录，临时文件
//     不落盘）+ 工作区可写 bind + 缓存目录（cacheDirs，cacheRoots 采集）
//     逐个可写 bind
//   - protectedReadonly：可写根下的敏感子路径（.git 等）以 --ro-bind 覆盖
//     为只读（bwrap 后绑定覆盖前绑定）
//   - read-only（level 1）：无任何可写挂载（/dev/null 由 --dev 提供）
func bwrapArgs(level int, workdir string, cacheDirs []string, protectedReadonly []string, argv []string) []string {
	args := []string{"bwrap", "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "--die-with-parent"}
	if level >= proto.LevelWrite {
		args = append(args, "--tmpfs", "/tmp")
		if workdir != "" {
			args = append(args, "--bind", workdir, workdir)
		}
		for _, d := range cacheDirs {
			if d != "" {
				args = append(args, "--bind", d, d)
			}
		}
		for _, p := range protectedReadonly {
			if p != "" {
				args = append(args, "--ro-bind", p, p)
			}
		}
	}
	return append(append(args, "--"), argv...)
}

// ---- darwin: Seatbelt (sandbox-exec) ----

// macosSeatbeltExecutable 是 sandbox-exec 的固定路径（防 PATH 注入：
// 若 /usr/bin/sandbox-exec 被篡改，攻击者已 root——codex 同策略）。
const macosSeatbeltExecutable = "/usr/bin/sandbox-exec"

// seatbeltArgs 构建 sandbox-exec 包装 argv。SBPL 为 allow-default +
// (deny file-write*) 白名单：read-only 仅放 /dev/null 字面量；
// workspace-write 追加工作区 + 缓存目录（cacheRoots）+ 平台临时区
// （/private/tmp 与 $TMPDIR），全部 canonicalize——Seatbelt 匹配 resolved path
// （/tmp 即 /private/tmp，必须消解后再匹配）；随后对可写根下的敏感
// 子路径（.git 等）追加 deny 规则（SBPL deny 优先于 allow，覆盖写白名单）。
// .git 覆盖的保护对象是 bash/rm 等通用命令——git 自身（isGitArgv）豁免，
// git 写操作的等级由 vcore 子命令分级表承担（§2.4）。
func seatbeltArgs(level int, workdir string, argv []string) []string {
	forms := []string{
		"(version 1)",
		"(allow default)",
		"(deny file-write*)",
		`(allow file-write* (literal "/dev/null"))`,
	}
	if level >= proto.LevelWrite {
		for _, root := range writableRoots(workdir) {
			forms = append(forms, "(allow file-write* (subpath "+sbplString(root)+"))")
		}
		if workdir != "" && !isGitArgv(argv) {
			for _, name := range protectedMetadataNames {
				p := filepath.Join(workdir, name)
				forms = append(forms, "(deny file-write* (subpath "+sbplString(canonicalRoot(p))+"))")
			}
		}
	}
	return append([]string{macosSeatbeltExecutable, "-p", stringsJoin(forms), "--"}, argv...)
}

func stringsJoin(forms []string) string {
	return strings.Join(forms, " ")
}

// writableRoots 收集 workspace-write 的全部可写根：平台临时区 + 工作区 +
// 常见工具链缓存目录（cacheRoots，go/npm/pip 等构建缓存——沙箱下不可写会
// 导致构建工具链不可用；投毒风险属可接受边界，见 host_sandbox.md）。
// canonicalize + 去重。
func writableRoots(workdir string) []string {
	roots := []string{"/private/tmp", os.TempDir()}
	if workdir != "" {
		roots = append(roots, workdir)
	}
	roots = append(roots, cacheRoots()...)
	seen := map[string]bool{}
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		if r == "" {
			continue
		}
		c := canonicalRoot(r)
		if seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	return out
}

// canonicalRoot 消解路径到真实文件系统身份（symlink/.. 展开；失败回落绝对路径）。
func canonicalRoot(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// isGitArgv 判定被包装命令是否为 git 自身（argv[0] basename 匹配）。
// .git 只读覆盖的保护对象是 bash/rm 等通用命令，git 不应被误伤；
// bash -c "git ..." 不豁免（走 git 命令本身，或审批 9 免沙箱）。
func isGitArgv(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	base := filepath.Base(argv[0])
	return base == "git" || base == "git.exe"
}

// existingDirs 过滤掉空串与不存在的目录（bwrap --bind / windows grantDirWrite
// 都要求源存在；seatbelt subpath 不强制，统一过滤保持三端一致）。
func existingDirs(dirs ...string) []string {
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if d == "" {
			continue
		}
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			out = append(out, d)
		}
	}
	return out
}

// sbplString 转义一个路径为 SBPL 字符串字面量。
func sbplString(p string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(p, `\`, `\\`), `"`, `\"`) + `"`
}

// mergeEnv 把附加环境并入继承环境：同名键以附加值为准（替换而非追加——
// Windows CreateProcess 对重复变量名取首个，必须过滤；unix 平台 plan.env
// 恒 nil，本函数仅 windows 受限令牌的 TMP/TEMP 注入使用）。
func mergeEnv(extra []string) []string {
	if len(extra) == 0 {
		return nil
	}
	drop := map[string]bool{}
	for _, kv := range extra {
		if i := strings.IndexByte(kv, '='); i > 0 {
			drop[kv[:i]] = true
		}
	}
	base := os.Environ()
	env := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		if i := strings.IndexByte(kv, '='); i > 0 && drop[kv[:i]] {
			continue
		}
		env = append(env, kv)
	}
	return append(env, extra...)
}
