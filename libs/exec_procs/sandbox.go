// Native processes enforce the host execution policy independently of runtime
// approval. Read/write allow rules are additive, deny always wins, and cwd grants
// no access. A backend that cannot enforce a rule rejects execution.
package exec_procs

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/netauth"
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

// confineSpec 是一次沙箱包装的完整输入（三域授权模型快照 + 等级/工作区/argv）。
// 快照语义：每次 Start 读当次值（set_config/grant 动态生效），已启动进程不回溯。
type confineSpec struct {
	level      int             // 授予等级（仅选择沙箱 profile）：1=read-only；2/3/4/9=workspace-write
	workdir    string          // 进程 cwd，不授予目录权限
	extra      []string        // 追加可写根（nil = 仅基础白名单）
	argv       []string        // 被包装命令
	deny       []string        // fs deny 预展开模式（fsauth.DenyPatterns 快照）
	readAllow  []string        // 展开的可读路径
	writeAllow []string        // 展开的可写 glob（裸路径由 extra 传入）
	fsOpen     bool            // fs_policy=open：写除 deny 全放（darwin allow file-write* / bwrap 整机 rw）
	netOpen    bool            // net_policy=open：不加网络规则
	netDeny    []netauth.Entry // net_deny 快照（恒优先于 allow）
	netAllow   []netauth.Entry // net allow 快照（含内建 localhost:* 与 sid 临时 grant）
}

// Confine 将 argv 包装为沙箱执行形态（返回替换 argv；windows 的实际
// confined 路径走 planConfined 的令牌注入，本函数仅供非 windows 调用与
// 统一测试）。extraWrite 为追加可写根（nil = 仅基础白名单）。
// level 为本次调用的授予等级（仅选择沙箱 profile）：1 = read-only；
// 2/3/4/9 = workspace-write；0 = 未设置/异常值，按 read-only 处理（fail-closed）。
// 审批通过（9）不豁免沙箱——免沙箱不经本函数表达（StartOptions.NoSandbox）。
// 无可用后端返回错误（fail-closed），绝不返回未包装 argv。
// deny/net 快照恒零值：现调用方仅测试；生产路径必须走 Start（StartOptions
// 授权快照字段），否则 deny 隔离与网络管控静默缺失。
func Confine(level int, workdir string, argv []string) ([]string, error) {
	plan, err := planConfined(confineSpec{level: level, workdir: workdir, argv: argv, netOpen: true})
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
// 云端批准不会豁免执行端必须落实的本地策略。
func sandboxUnavailable(level int) error {
	return fmt.Errorf(
		"sandbox: level %d requires confinement but no sandbox backend is usable on this host "+
			"(install bubblewrap on Linux); the command was not run", level)
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

func bwrapArgs(spec confineSpec, cacheDirs []string, protectedReadonly []string) []string {
	// fs_policy=open（写级）：整机只读改整机可写（deny 覆盖挂载仍在后追加，恒优先）。
	args := []string{"bwrap"}
	if spec.fsOpen {
		flag := "--ro-bind"
		if spec.level >= proto.LevelWrite {
			flag = "--bind"
		}
		args = append(args, flag, "/", "/")
	} else {
		args = append(args, "--tmpfs", "/")
		for _, pat := range spec.readAllow {
			root := strings.TrimSuffix(pat, "/**")
			if _, err := os.Stat(root); err == nil {
				args = append(args, "--ro-bind", root, root)
			}
		}
	}
	args = append(args, "--dev", "/dev", "--proc", "/proc", "--die-with-parent")
	if !spec.netOpen {
		// net_policy=deny：--unshare-net 全断（新 net ns 仅 loopback 且未配置——
		// loopback 也不可用）。bwrap 无 per-destination 规则引擎，白名单粒度
		// linux 不生效（近似层，net_allow 仅 darwin 落地；见 host_sandbox.md）。
		args = append(args, "--unshare-net")
	}
	args = append(args, rlimitArgs()...)
	if spec.level >= proto.LevelWrite {
		args = append(args, "--tmpfs", "/tmp")
		for _, d := range append(append([]string{}, cacheDirs...), literalWriteRoots(spec.writeAllow)...) {
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
	// readAllow（系统 CA）并入覆盖判定：deny 实例化当前不涉系统路径
	//（** 开头 $HOME 锡定 + 字面表）——并入为对齐/未来防护。
	args = append(args, bwrapDenyArgs(spec.deny)...)
	return append(append(args, "--"), spec.argv...)
}

// bwrapDenyArgs overlays denied paths after allow mounts. Policy validation
// rejects scopes the mount backend cannot fully enforce before this is called.
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

// seatbeltArgs emits explicit read/write scopes, followed by unconditional denies.
func seatbeltArgs(spec confineSpec) []string {
	forms := []string{
		"(version 1)",
		"(allow default)",
		"(deny file-write*)",
		`(allow file-write* (literal "/dev/null"))`,
	}
	if !spec.fsOpen {
		forms = append(forms, "(deny file-read*)")
		// dyld opens the root directory as an openat base; this literal rule
		// permits that directory only, never its descendants.
		forms = append(forms, `(allow file-read-data (literal "/"))`)
		// Runtime path traversal needs metadata on ancestors of readable roots.
		// This grants no directory listing or file contents outside those roots.
		for _, root := range readAncestors(spec.readAllow) {
			forms = append(forms, "(allow file-read-metadata (literal "+sbplString(root)+"))")
		}
		for _, pat := range spec.readAllow {
			forms = append(forms, "(allow file-read* (regex "+sbplString(globToSBPLRegex(pat))+"))")
		}
	}
	if spec.level >= proto.LevelWrite {
		if spec.fsOpen {
			// fs_policy=open：写全放（deny 表在后输出，恒优先）。
			forms = append(forms, "(allow file-write*)")
		} else {
			for _, root := range spec.extra {
				forms = append(forms, "(allow file-write* (subpath "+sbplString(root)+"))")
			}
			for _, pat := range spec.writeAllow {
				forms = append(forms, "(allow file-write* (regex "+sbplString(globToSBPLRegex(pat))+"))")
			}
		}
	}
	forms = append(forms, seatbeltNetForms(spec)...)
	for _, pat := range spec.deny {
		if pat == "" {
			continue
		}
		re := sbplString(globToSBPLRegex(pat))
		forms = append(forms, "(deny file-read* (regex "+re+"))")
		forms = append(forms, "(deny file-write* (regex "+re+"))")
		forms = append(forms, "(deny network-outbound (remote unix (regex "+re+")))")
	}
	if spec.level >= proto.LevelWrite && spec.workdir != "" && !isGitArgv(spec.argv) {
		for _, name := range protectedMetadataNames {
			p := filepath.Join(spec.workdir, name)
			forms = append(forms, "(deny file-write* (subpath "+sbplString(canonicalRoot(p))+"))")
		}
	}
	return append([]string{macosSeatbeltExecutable, "-p", stringsJoin(forms), "--"}, spec.argv...)
}

// seatbeltNetForms 生成网络管控段（net_policy=deny 锁定模式时；open 模式零规则，
// allow default 兜底）：
//
//	(deny network-inbound)(deny network-outbound) 打底
//	→ net_allow 逐条放行（2026-09-07 实测：seatbelt 网络过滤器 host 只支持
//	  */localhost——按目标 IP/域名的内核级放行不存在；loopback 条目发
//	  localhost 两形态（remote tcp 连通 + local tcp inbound bind/listen）；
//	  非 loopback 条目退化为 *:port 按端口粗放行；port=* 不可表达跳过——
//	  精细 host:port 判定由 curl 虚拟指令工具层承担（shellCurlFetcher 前置
//	  netauth.Allowed 闸 + --resolve 钉住，见 fetcher.go））
//	→ net_deny 逐条 deny（恒优先——过滤规则与兜底 deny 共存实测互不干扰）；
//	  仅 loopback 条目落地（对抗内建 localhost:* allow）；非 loopback deny
//	  不输出（基线全拒已覆盖，host 粒度内核不可表达，工具层 curl 闸精判
//	  兜底——避免 *:port 株连同端口 allow 目标，见 seatbeltEntryForms）
//
// 实测纪律（2026-09-07 探针）：
//   - `(allow network-outbound (local tcp "localhost:*"))` 是毒形态——其语义
//     覆盖一切出站连接（本地端恒命中），等于拆掉整个 deny outbound，严禁输出；
//   - `(remote unix ...)` 必须 regex 形态（裸字符串参数非法）；
//   - deny network* 下系统 DNS（mDNSResponder/dnssd mach+XPC+unix socket
//     组合）实测多轮放行均不生效——锁定模式等于无沙箱内 DNS，FQDN 目标由
//     工具层 pod 侧解析 + curl --resolve 钉住补偿；
//   - 规则顺序对本段不重要（过滤规则与兜底 deny 各测其序），但 deny 表（文件/
//     unix socket）仍在本段之后输出（旧教训不回收）。
func seatbeltNetForms(spec confineSpec) []string {
	if spec.netOpen {
		var forms []string
		for _, e := range spec.netDeny {
			forms = append(forms, seatbeltEntryForms(e, "deny")...)
		}
		return forms
	}
	forms := []string{
		"(deny network-inbound)",
		"(deny network-outbound)",
	}
	for _, e := range spec.netAllow {
		forms = append(forms, seatbeltEntryForms(e, "allow")...)
	}
	for _, e := range spec.netDeny {
		forms = append(forms, seatbeltEntryForms(e, "deny")...)
	}
	return forms
}

// seatbeltEntryForms 把一条 netauth 条目实例化为 SBPL 规则（粒度限制与实测
// 纪律见 seatbeltNetForms 注释）：
//   - loopback（localhost/127.0.0.1/::1）：allow 发 remote tcp（连通）+
//     local tcp inbound（bind/listen，本机服务场景）两形态；deny 仅 remote tcp
//     （对抗内建 localhost:* allow，localhost 内核可精判）；
//   - 非 loopback allow：退化为 *:port（host 必须为 */localhost，按端口放行）；
//     port=* 内核不可表达，跳过（工具层 curl 闸仍按 host:port 精判）；
//   - 非 loopback deny：不输出内核规则——锁定模式基线本就是全拒，此类规则
//     唯一作用是对抗同端口 allow 的 *:port 粗放行，但内核无 host 粒度必然
//     株连同端口的 allow 目标（显式放行被无关 deny 打死，2026-09-07 评审）；
//     精细 host 判定由工具层 curl 闸承担，不假装内核能表达。
func seatbeltEntryForms(e netauth.Entry, verb string) []string {
	if isLoopbackHost(e.Host) {
		addr := sbplString("localhost:" + e.Port)
		if verb == "deny" {
			return []string{"(deny network-outbound (remote tcp " + addr + "))"}
		}
		return []string{
			"(allow network-outbound (remote tcp " + addr + "))",
			"(allow network-inbound (local tcp " + addr + "))",
		}
	}
	if verb == "deny" || e.Port == "*" {
		return nil
	}
	return []string{"(allow network-outbound (remote tcp " + sbplString("*:"+e.Port) + "))"}
}

// isLoopbackHost 判定 loopback 条目（localhost 字面或 IPv4/IPv6 loopback）。
func isLoopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	if ip, err := netip.ParseAddr(h); err == nil {
		return ip.IsLoopback()
	}
	return false
}

func stringsJoin(forms []string) string {
	return strings.Join(forms, " ")
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

// validateProcessPolicy fails before execution when a backend cannot enforce the
// requested scope. Never approximate host rules by opening all hosts or paths.
func validateProcessPolicy(spec confineSpec, platform string) error {
	fail := func(why string) error {
		return &proto.DeniedError{Reason: "sandbox cannot enforce host policy: " + why}
	}
	if platform == "windows" && (!spec.fsOpen || len(spec.deny) > 0 || !spec.netOpen || len(spec.netDeny) > 0) {
		return fail("this Windows backend does not implement path read restrictions or network rules")
	}
	for _, e := range spec.netDeny {
		if platform != "darwin" || !isLoopbackHost(e.Host) {
			return fail("selective network deny is unsupported by this backend")
		}
	}
	if !spec.netOpen {
		for _, e := range spec.netAllow {
			if platform != "darwin" || !isLoopbackHost(e.Host) {
				return fail("selective network allow is unsupported by this backend")
			}
		}
	}
	if platform == "linux" {
		if len(spec.deny) > 0 {
			return fail("Linux mount confinement cannot enforce path deny rules")
		}
		for _, p := range append(append(append([]string{}, spec.readAllow...), spec.writeAllow...), spec.deny...) {
			if strings.ContainsAny(strings.TrimSuffix(p, "/**"), "*?[") {
				return fail("Linux mount confinement cannot enforce this path glob: " + p)
			}
		}
	}
	return nil
}

func literalWriteRoots(patterns []string) []string {
	roots := make([]string, 0, len(patterns))
	for _, p := range patterns {
		roots = append(roots, strings.TrimSuffix(p, "/**"))
	}
	return roots
}

func readAncestors(patterns []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range patterns {
		if i := strings.IndexAny(p, "*?"); i >= 0 {
			p = p[:i]
		}
		for p = filepath.Dir(p); p != "."; p = filepath.Dir(p) {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
			if p == "/" {
				break
			}
		}
	}
	return out
}
