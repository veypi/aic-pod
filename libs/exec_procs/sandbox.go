// 进程沙箱（§5.10）：exec 外部进程统一经沙箱包装执行。
//
// 规则：未显式免沙箱（StartOptions.NoSandbox）的进程调用一律进沙箱——
// 审批通过（LevelApproved 9）也不例外：9 只是等级语义，免沙箱唯一通道是
// 显式 nosandbox（外部请求须经人工审批，required Critical(4) ⇒ 必审批）。
// level 1 = read-only（除 /dev/null 外不可写）；level 2/3/4/9 = workspace-write
// （仅工作区 + 常见工具链缓存目录 cacheRoots + 平台临时区 + 公共区 $HOME/.aic 可写）；
// 0 = 未设置/异常值，按 read-only 兜底。
// 无可用后端时 fail-closed：拒绝执行，绝不静默裸跑。
// deny 隔离（2026-09-05）：默认读全开，fsauth deny 表（DenyPatterns）经
// StartOptions.DenyPaths 生成拒绝规则——darwin seatbelt 文件读写双拒 + unix
// connect 拒绝（deny file-read*/file-write* regex，另加 network-outbound
// (remote unix (regex ...))——AF_UNIX connect 不走 file-* 判定，实测 docker.sock
// 文件操作全被拒而 curl --unix-socket 直通）、linux bwrap 覆盖挂载（文件/
// socket 读写双拒 / 目录读黑洞＋写入不落地）——fs 与 exec 共用同一份 deny 名单；
// windows 侧不实现（restricting 集初始化依赖，见 sandbox_windows.go 注释）。
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
//
// 资源限制（2026-08-28 补齐，与文件隔离正交）：
//   - linux: bwrap --rlimit（bwrap 原生，零额外进程）；
//   - darwin: sh ulimit 包装（Seatbelt 不支持资源限制；RLIMIT 跨 exec 继承）；
//   - windows: Job Object（进程内存 4GiB / job 内存 8GiB / 活动进程 256）。
//
// 三端同一组上限（resourceLimit* 常量），read-only 与 workspace-write 同限——
// 此前只有文件隔离，沙箱内命令可无限分配内存/派生进程，实测打爆系统内存死机。
package exec_procs

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/fsauth"
	"github.com/veypi/aic-pod/libs/proto"
)

// probeTimeout 是每个后端功能性 probe 的超时（真跑一次最小命令验证；
// 0 会被视为无超时，故必须为正数）。
const probeTimeout = 5 * time.Second

// 沙箱资源上限（三端统一语义，与文件隔离正交；read-only 与 workspace-write 同限）：
//   - AS 4GiB：防大 malloc 吃满物理内存 + swap 导致系统假死（死机事故根因）
//   - job 内存 8GiB：windows Job Object 整个 job（含全部子孙）合计上限
//   - NOFILE 1024：防 fd 耗尽
//   - CPU 600s：与 30m wall-clock 超时双保险
//   - FSIZE 1GiB：防单文件写爆磁盘（write 级沙箱）
//   - CORE 0：禁 core dump 落盘
//
// 不设 RLIMIT_NPROC（2026-08-31 事故修正）：unix 内核按 real-UID 全系统计数，
// 计数 >= 上限即 fork EAGAIN——桌面开发机单 UID 常驻进程数百个（本机实测约
// 600），设 256 等于禁掉沙箱内一切 fork，全部命令瘫痪。防 fork 炸弹由既有机制
// 兜底：30m wall 超时 + 进程组 killEntry 全灭 + CPU 600s。windows Job Object
// 的活动进程上限（resourceLimitJobProcesses）是 job 级计数（语义正确）不受影响，
// 仍在 sandbox_windows.go。
const (
	resourceLimitAS           = 4 << 30
	resourceLimitJobMemory    = 8 << 30
	resourceLimitJobProcesses = 256
	resourceLimitNoFile       = 1024
	resourceLimitCPU          = 600
	resourceLimitFSize        = 1 << 30
)

// protectedMetadataNames 是工作区可写时仍保持只读的敏感子路径名
// （借鉴 codex：.git 防 AI 破坏仓库元数据/历史；可扩展 .agents 等）。
// 沙箱内 git 写操作（add/commit）会失败——AI 可经审批 9 免沙箱执行。
// linux 有效性实测（2026-08-14，bwrap / Debian 13）：沙箱内嵌套
// `unshare -rm` 后 umount ro-bind 覆盖、re-bind 工作区两种逃逸均被内核
// 挂载归属规则拒绝（挂载属于父 userns，嵌套 ns 内无 CAP_SYS_ADMIN 可操作）
// ——ro-bind 覆盖是有效边界，非纸面加固。
var protectedMetadataNames = []string{".git"}

// publicRoots 返回公共可写区（$HOME/.aic，cfg.PublicDir）：workspace-write
// 白名单成员之一，与工作区/缓存/临时区并列——AI 跨会话保存产物与工具状态
// 落盘（browser state save 等）共用；目录首次调用时创建；获取失败返回 nil
// （白名单宁缺毋滥，不可写比错误可写安全）。
func publicRoots() []string {
	p, err := cfg.PublicDir()
	if err != nil {
		return nil
	}
	return []string{p}
}

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
//   - job：windows Job Object 句柄（资源限制：内存/进程数；其他平台恒 0；
//     spawn 成功后由 exec_procs assign 子进程，进程结束后随 cleanup 关闭）
//   - env：附加环境变量（windows：TMP/TEMP 指向私有临时目录）
//   - cleanup：进程结束后调用（windows：撤销私有临时目录 ACE 并删除 +
//     关闭 Job Object；其他平台 nil）
type launchPlan struct {
	argv    []string
	token   uintptr
	job     uintptr
	env     []string
	cleanup func()
}

var (
	sandboxMu      sync.Mutex
	sandboxVerdict sandboxBackend = -1 // -1 = 未探测
)

// Confine 将 argv 包装为沙箱执行形态（返回替换 argv；windows 的实际
// confined 路径走 planConfined 的令牌注入，本函数仅供非 windows 调用与
// 统一测试）。extraWrite 为追加可写根（nil = 仅基础白名单）。
// level 为本次调用的授予等级（仅选择沙箱 profile）：1 = read-only；
// 2/3/4/9 = workspace-write；0 = 未设置/异常值，按 read-only 处理（fail-closed）。
// 审批通过（9）不豁免沙箱——免沙箱不经本函数表达（StartOptions.NoSandbox）。
// 无可用后端返回错误（fail-closed），绝不返回未包装 argv。
// deny 模式恒 nil：现调用方仅测试；生产路径必须走 Start（StartOptions.DenyPaths），
// 否则 deny 隔离静默缺失。
func Confine(level int, workdir string, argv []string) ([]string, error) {
	plan, err := planConfined(level, workdir, nil, argv, nil)
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
//
// rlimitArgs 构建 bwrap 资源限制参数段（--rlimit TYPE VALUE ...）。
// bwrap 在 exec 前对子进程 setrlimit（soft=hard），与文件隔离正交、
// read-only 与 workspace-write 同限（resourceLimit* 常量统一语义）。
func rlimitArgs() []string {
	return []string{
		"--rlimit", "AS", strconv.FormatUint(uint64(resourceLimitAS), 10),
		"--rlimit", "NOFILE", strconv.Itoa(resourceLimitNoFile),
		"--rlimit", "CPU", strconv.Itoa(resourceLimitCPU),
		"--rlimit", "FSIZE", strconv.FormatUint(uint64(resourceLimitFSize), 10),
		"--rlimit", "CORE", "0",
	}
}

// confineRlimits 构造 sh ulimit 包装 argv（darwin planConfined 使用，
// 测试跨平台直接引用）。Seatbelt 不支持资源限制，包一层 /bin/sh：
// ulimit 设置的 RLIMIT 跨 exec 继承（sandbox-exec 与其最终命令同受约束），
// 且子进程只能降低不能提高；ulimit 失败即退出（fail-closed，命令不执行）。
// 注意：macOS 内核不支持 RLIMIT_AS（-v）/RLIMIT_DATA（-d），setrlimit 恒
// EINVAL（实测 2026-08-28）——大内存分配由 exec_procs 进程组 RSS 监控
// 兜底（rss_darwin.go）。不设 -u（RLIMIT_NPROC 按 UID 全系统计数，桌面
// 常驻进程即超限，见常量块注释）。单位：-n fd、-t CPU 秒、-f 512B 块、-c core。
func confineRlimits(argv []string) []string {
	script := fmt.Sprintf(
		"ulimit -n %d -t %d -f %d -c 0 2>/dev/null || exit 1; exec \"$@\"",
		resourceLimitNoFile, resourceLimitCPU, resourceLimitFSize>>9)
	return append([]string{"/bin/sh", "-c", script, "sh"}, argv...)
}

func bwrapArgs(level int, workdir string, cacheDirs []string, protectedReadonly []string, argv []string, deny []string) []string {
	args := []string{"bwrap", "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "--die-with-parent"}
	args = append(args, rlimitArgs()...)
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
	// deny 隔离覆盖（§5.10）：后挂载优先（bwrap 后绑定覆盖前绑定），
	// 追加在全部 bind 之后；read-only 与 workspace-write 同隔离。
	args = append(args, bwrapDenyArgs(deny)...)
	return append(append(args, "--"), argv...)
}

// bwrapDenyArgs 把 deny 模式实例化为 bwrap 覆盖挂载参数。
// bwrap 无路径规则引擎，只能挂载覆盖已存在路径：
//   - 目录 → --tmpfs 覆盖（原内容不可见；写入落入临时 tmpfs 不落地）
//   - 文件 → --ro-bind /dev/null 覆盖（读得空文件，写被 EROFS 拒绝）
//
// 实例化粒度（存在性判定每次 Start 实时执行——新创建文件下次覆盖）：
//   - 纯字面路径：stat 判定目录/文件；不存在跳过（不可读无害）
//   - 尾 /**（或 /*）：剥尾段得目录，按目录处理（覆盖整树含未来创建）
//   - 段内 glob（* ?，无 **）：字面前缀目录 readdir 逐项匹配（filepath.Match
//     段语义）覆盖；无字面前缀（** 开头）→ $HOME 根级锚定：最后一个非 **
//     段在 home 直接子级匹配（.ssh 类目录/id_ed25519* 文件/**.key）
//   - 无法实例化（中间 **、含 [ 字符类语法）→ 跳过：exec 通道无兜底（fsauth
//     判定层仅约束 fs 工具/VFS，拦不住进程内 cat）；bwrap deny 隔离是近似层，
//     完整 glob 语义仅 seatbelt。
func bwrapDenyArgs(deny []string) []string {
	var args []string
	for _, pat := range deny {
		if pat == "" {
			continue
		}
		for _, p := range denyCoverTargets(pat) {
			args = append(args, overlayArgs(p)...)
		}
	}
	return args
}

// overlayArgs 返回单目标路径的覆盖挂载参数（目录 tmpfs / 普通文件与 unix
// socket 以 /dev/null ro-bind 覆盖；其余（设备/不存在）跳过）。socket 覆盖 =
// linux 侧 AF_UNIX connect 隔离：覆盖后 connect 只见到 /dev/null 直接失败
// （对齐 darwin seatbelt 的 network-outbound 规则；修复前 socket 被跳过，
// docker.sock 可直通，实测 2026-09-05）。
func overlayArgs(p string) []string {
	if p == "" {
		return nil
	}
	st, err := os.Stat(p)
	if err != nil {
		return nil
	}
	if st.IsDir() {
		return []string{"--tmpfs", p}
	}
	if st.Mode().IsRegular() || st.Mode()&os.ModeSocket != 0 {
		return []string{"--ro-bind", "/dev/null", p}
	}
	return nil
}

// denyCoverTargets 把一条 deny 模式实例化为目标路径列表。
func denyCoverTargets(pat string) []string {
	pat = filepath.ToSlash(pat)
	if !strings.ContainsAny(pat, "*?") {
		return []string{pat}
	}
	// ** 开头（无字面前缀）：home 根级锚定——此形态的字面前缀为空，
	// 必须优先于尾段剥离（**/.ssh/** 剥尾会得到伪目录 **/.ssh）
	if strings.HasPrefix(pat, "**/") {
		return homeAnchorTargets(pat)
	}
	// 尾 /** 或 /*：剥尾段得目录（前缀必须无 glob——否则仍是跨段形态）
	if idx := strings.LastIndex(pat, "/"); idx >= 0 && !strings.Contains(pat[idx+1:], "/") && strings.Contains(pat[idx+1:], "*") {
		// 尾段整段是 * 或 **（无其它字符）且前缀无通配 → 目录级
		tail := pat[idx+1:]
		if (tail == "*" || tail == "**") && !strings.ContainsAny(pat[:idx], "*?") {
			return []string{pat[:idx]}
		}
	}
	// 段内 glob（含 ** 但非尾整段 **）→ 检查是否有 `**` 或 `[`：无法实例化
	if strings.Contains(pat, "**") || strings.ContainsAny(pat, "[]") {
		return nil
	}
	// 无 ** 的单 glob：字面前缀 readdir 枚举
	segs := strings.Split(pat, "/")
	lit := 0
	for _, s := range segs {
		if strings.ContainsAny(s, "*?") {
			break
		}
		lit++
	}
	if lit == 0 || lit >= len(segs) {
		return nil
	}
	prefix := strings.Join(segs[:lit], "/")
	if strings.HasPrefix(pat, "/") {
		prefix = "/" + prefix
	}
	st, err := os.Stat(prefix)
	if err != nil || !st.IsDir() {
		return nil
	}
	want := segs[lit] // 仅支持单 glob 段（其余段须字面）
	for _, rest := range segs[lit+1:] {
		if strings.ContainsAny(rest, "*?") {
			return nil
		}
	}
	entries, err := os.ReadDir(prefix)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if ok, _ := filepath.Match(want, e.Name()); ok {
			out = append(out, filepath.Join(prefix, e.Name()))
		}
	}
	return out
}

// homeAnchorTargets 把 ** 开头模式锚定到 $HOME 根级：最后一个非 ** 段
// 为目录名或段内 glob（filepath.Match 匹配 home 直接子级）。
func homeAnchorTargets(pat string) []string {
	segs := strings.Split(pat, "/")
	var anchor string
	for i := len(segs) - 1; i >= 0; i-- {
		if segs[i] != "**" && segs[i] != "" {
			anchor = segs[i]
			break
		}
	}
	if anchor == "" {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if ok, _ := filepath.Match(anchor, e.Name()); ok {
			out = append(out, filepath.Join(home, e.Name()))
		}
	}
	return out
}

// ---- darwin: Seatbelt (sandbox-exec) ----

// macosSeatbeltExecutable 是 sandbox-exec 的固定路径（防 PATH 注入：
// 若 /usr/bin/sandbox-exec 被篡改，攻击者已 root——codex 同策略）。
const macosSeatbeltExecutable = "/usr/bin/sandbox-exec"

// seatbeltArgs 构建 sandbox-exec 包装 argv。SBPL 为 allow-default +
// (deny file-write*) 白名单：read-only 仅放 /dev/null 字面量；
// workspace-write 追加工作区 + 缓存目录（fsauth.CacheRoots）+ 平台临时区
// （/private/tmp 与 $TMPDIR）+ 追加根（extra），全部 canonicalize——Seatbelt
// 匹配 resolved path（/tmp 即 /private/tmp，必须消解后再匹配）；随后对可写根下
// 的敏感子路径（.git 等）追加 deny 规则（SBPL deny 优先于 allow，覆盖写白名单）。
// .git 覆盖的保护对象是 bash/rm 等通用命令——git 自身（isGitArgv）豁免，
// git 写操作的等级由 vcore 子命令分级表承担（§2.4）。
//
// deny 追加拒绝规则（fsauth.DenyPatterns 预展开模式，§5.10 deny 隔离）：
// 每条模式经 globToSBPLRegex 转 SBPL (regex ...) 规则，file-read* 与 file-write*
// 各出一条读写双拒（修复前仅拒读，纯写打开仍可改写可写根内 deny 文件，实测
// 2026-09-05；与 linux 覆盖挂载事实行为、fsauth deny=(0,0) 语义对齐），另加一条
// network-outbound (remote unix (regex ...))——AF_UNIX connect() 不走 file-* 判定
// （实测 2026-09-05：deny 条目 stat/读/写全拒而 curl --unix-socket 直通，
// docker.sock = 主机逃逸）；SBPL network 过滤器 (remote unix (regex ...)) 实测可用，
// 内核对判定路径先规范化（/tmp→/private/tmp symlink 亦命中，实测）。
// **规则顺序：deny 表在写白名单之后输出**——SBPL 后匹配覆盖先匹配，先输出时
// 可写根内 deny 条目（工作区的 **/.env / *.key 等）写保护会被 allow subpath
// 覆盖（.git 覆盖幸存仅因其在白名单后输出；实测修复 2026-09-05）。
// 字面路径以双形态进入名单（compileDeny 同时输出 canonical 与字面形）：
// 模式自身是符号链接时（/var/run/docker.sock → 厂商 socket）两形态都须命中。
// SBPL 无 glob filter，regex 为 POSIX ERE 且对完整路径字符串匹配（子串命中，锚定 ^ 有效）；
// seatbelt 判定前做路径规范化（大小写变体/.SSH、symlink 跳转均被拒，实测）。字面路径直接 (regex) 亦可用，但统一走转换器保持单一路径。
func seatbeltArgs(level int, workdir string, extra []string, argv []string, deny []string) []string {
	forms := []string{
		"(version 1)",
		"(allow default)",
		"(deny file-write*)",
		`(allow file-write* (literal "/dev/null"))`,
	}
	if level >= proto.LevelWrite {
		for _, root := range writableRoots(workdir, extra) {
			forms = append(forms, "(allow file-write* (subpath "+sbplString(root)+"))")
		}
	}
	for _, pat := range deny {
		if pat == "" {
			continue
		}
		re := sbplString(globToSBPLRegex(pat))
		forms = append(forms, "(deny file-read* (regex "+re+"))")
		forms = append(forms, "(deny file-write* (regex "+re+"))")
		forms = append(forms, "(deny network-outbound (remote unix (regex "+re+")))")
	}
	if level >= proto.LevelWrite && workdir != "" && !isGitArgv(argv) {
		for _, name := range protectedMetadataNames {
			p := filepath.Join(workdir, name)
			forms = append(forms, "(deny file-write* (subpath "+sbplString(canonicalRoot(p))+"))")
		}
	}
	return append([]string{macosSeatbeltExecutable, "-p", stringsJoin(forms), "--"}, argv...)
}

func stringsJoin(forms []string) string {
	return strings.Join(forms, " ")
}

// writableRoots 收集 workspace-write 的全部可写根：平台临时区 + 工作区 +
// 常见工具链缓存目录（fsauth.CacheRoots，go/npm/pip 等构建缓存——沙箱下不可写会
// 导致构建工具链不可用；投毒风险属可接受边界，见 host_sandbox.md）+ 公共区
// $HOME/.aic（publicRoots，AI 与工具状态共享保存区）+ 追加根（fsauth 配置
// 白名单/临时 grant，v0.14.5 统一名单）。canonicalize + 去重。
func writableRoots(workdir string, extra []string) []string {
	roots := []string{"/private/tmp", os.TempDir()}
	if workdir != "" {
		roots = append(roots, workdir)
	}
	roots = append(roots, fsauth.CacheRoots()...)
	roots = append(roots, publicRoots()...)
	roots = append(roots, extra...)
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

// globToSBPLRegex 把 fsauth glob 模式（matchPattern 语义）转 SBPL POSIX ERE
// （SBPL 无 glob filter；regex 对完整路径字符串匹配，锚定 ^ 有效，实测 2026-09-05）。
// 转换规则：
//   - ** → 跨段（保 fsauth 的零段语义，吸收紧邻的分隔符）：
//   - ** 段首（无前导 /）→ (.*)?（零/多段，可跨 /）
//   - ** 段尾（前导 / 被吸收）→ (/.*)?（零段 = 自身，其后任意）
//   - ** 段中（前导 / 吸收且**後跟 / 一并消费）→ /(.*/)?（零段 = 单分隔，多段 = 跨段）
//   - *  → [^/]*       段内任意字符（不含 /）
//   - ?  → [^/]        段内单字符
//   - 其余字符 → 转义字面（regexp 特殊字符加 \）
//
// 整串锚定（^...$）保证与 fsauth 段匹配同为「全路径匹配」：正因 ^...$ 锚定，
// .ssh2 类粘连名不会命中 **/.ssh/**（字面段后必须是串尾或 /）。残余偏差仅
// 一处：段内 **（a**b 非标准形态）经 (.*)? 展开可跨段，而 fsauth matchOne
// 不跨段 → 超集拒绝（安全方向，该形态本身即非法输入）。
func globToSBPLRegex(pat string) string {
	pat = filepath.ToSlash(pat)
	var b strings.Builder
	b.WriteByte('^')
	for i := 0; i < len(pat); {
		if strings.HasPrefix(pat[i:], "**") {
			if i > 0 && pat[i-1] == '/' {
				// 吸收已输出的前导 '/'：** 零段 = 无该分隔符
				s := b.String()
				b.Reset()
				b.WriteString(s[:len(s)-1])
				if i+2 >= len(pat) {
					b.WriteString("(/.*)?") // 尾段 **
				} else if pat[i+2] == '/' {
					b.WriteString("/(.*/)?") // 中段 **：消费紧随的 /（零段=单分隔）
					i++
				} else {
					b.WriteString("/(.*)?") // 中段 ** 无尾 /（非标准分隔形态）
				}
				i += 2
				continue
			}
			b.WriteString("(.*)?")
			i += 2
			continue
		}
		switch pat[i] {
		case '*':
			b.WriteString("[^/]*")
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexEscape(pat[i]))
		}
		i++
	}
	b.WriteByte('$')
	return b.String()
}

// regexEscape 转义一个字节为 POSIX ERE 字面量（返回非空串）。
func regexEscape(c byte) string {
	if c == '\\' || strings.ContainsRune(".+*?()[]{}^$|", rune(c)) {
		return "\\" + string(rune(c))
	}
	return string(rune(c))
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
