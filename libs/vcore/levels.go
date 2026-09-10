package vcore

import (
	"encoding/json"
	"strings"

	"github.com/veypi/aic-pod/libs/proto"
)

// 权限分级表（§2.4 required 标准表）作为数据与指令定义同包——
// server 事前检查与 host 纵深检查读同一张表，禁止各自另写。

// FSRequired 返回 fs action 的 required level（§2.4）。
func FSRequired(action string) int {
	switch action {
	case "read", "ls", "rg":
		return proto.LevelRead
	case "write", "edit", "cp", "mv", "rm":
		return proto.LevelWrite
	}
	return proto.LevelDanger // 未声明兜底
}

// FSRequiredIn 是 FSRequired 的环境感知版本：rm recursive 删除非空目录
// 动态提升至 Danger(3)（§2.4：不可逆破坏性）。data 为 fs JSON 参数原文。
func FSRequiredIn(env *Env, action string, data []byte) int {
	lv := FSRequired(action)
	if action != "rm" || env == nil || env.VFS == nil {
		return lv
	}
	var p struct {
		Path      string `json:"path"`
		Recursive bool   `json:"recursive"`
	}
	_ = json.Unmarshal(data, &p)
	if !p.Recursive || p.Path == "" {
		return lv
	}
	abs, err := env.Resolve(p.Path)
	if err != nil {
		return lv // 判定失败按 Write，由执行路径报真正的错误
	}
	entries, err := env.VFS.ReadDir(abs)
	if err == nil && len(entries) > 0 {
		return proto.LevelDanger
	}
	return lv
}

// execCoreLevels 是核心虚拟指令的静态 required level。
var execCoreLevels = map[string]int{
	"curl": proto.LevelWrite,

	"bg_list":  proto.LevelRead,
	"bg_wait":  proto.LevelRead,
	"commands": proto.LevelRead,
	"bg_kill":  proto.LevelDanger,

	// grant（统一授权申请，fs/net/ssh 三域）：必人工审批
	//（Critical 4 = 用户不可直接授予 ⇒ 必转审批；批准后 granted 9 生效）。
	// 实现为 host 特化（写 host 配置/内存授权表），vcore 只声明元数据与等级。
	"grant": proto.LevelCritical,

	// ssh 一级工具：远端命令执行，base = Danger(3)；目标闸（ssh 域 Policy）
	// 是独立硬条件，在 host dispatch 强制（不依赖等级）。
	"ssh": proto.LevelDanger,

	// scp 一级工具：本机↔远端文件拷贝，base = Danger(3) 同 ssh；目标闸同
	// ssh 域，本地侧 fsauth 门控（deny 0/0），均在 host dispatch 强制。
	"scp": proto.LevelDanger,
}

// gitSubLevels 是 git 子命令分级（§2.4）。
var gitSubLevels = map[string]int{
	"status": proto.LevelRead,
	"log":    proto.LevelRead,
	"diff":   proto.LevelRead,
	"branch": proto.LevelRead,

	"init":   proto.LevelWrite,
	"clone":  proto.LevelWrite,
	"add":    proto.LevelWrite,
	"commit": proto.LevelWrite,
	"pull":   proto.LevelWrite,
	// 切分支（git 自身拒覆盖未提交修改，安全）；pathspec 形态（checkout -- <path>，
	// 丢弃工作区修改）由 gitRequired 检测 "--" 提升至 Danger——与 restore/reset 同级
	"checkout": proto.LevelWrite,
	"switch":   proto.LevelWrite, // checkout 的切分支现代同义词

	"push":  proto.LevelDanger, // 外发远端
	"reset": proto.LevelDanger, // 可丢弃本地修改
}

// gitValueFlags 是 git 带值 flag 已知表（§5.5：子命令判定先跳过带值 flag）。
var gitValueFlags = map[string]bool{"-C": true, "-c": true}

// browserSubLevels 是 browser 子命令分级（§2.4，自有指令集——仅声明端存在：
// 浏览器插件 / desktop 壳原生 JS 实现，见 aic-pod/browser/src/tools/browser/）。
// 读类 Read(1)；其余页面交互统一 Write(2) 基线（§2.4：click 逐次确认会使
// 浏览自动化不可用）。
var browserSubLevels = map[string]int{
	"snapshot":   proto.LevelRead,
	"read":       proto.LevelRead,
	"get":        proto.LevelRead,
	"screenshot": proto.LevelRead,
	"network":    proto.LevelRead,
}

// cuaSubLevels 是 cua 子命令分级（§2.4，desktop 壳 provider：cua-driver MCP 桥接）。
// 读类 Read(1)；窗口内交互 Write(2) 基线（与 browser 同理：逐次确认会使自动化不可用）；
// --delivery foreground 与 --scope desktop 由 cuaRequired 提级 Danger(3)——
// 前台接管/真实鼠标接管都是用户可见的越界行为，逐次审批；动作默认走驱动后台
// 精确路由（方案 v3，窗口本地指针）。
var cuaSubLevels = map[string]int{
	"doctor":        proto.LevelRead,
	"apps":          proto.LevelRead,
	"windows":       proto.LevelRead,
	"snapshot":      proto.LevelRead,
	"browser-state": proto.LevelRead,
	// cursor：agent 光标浮层控制（on/off/state/motion/theme）——纯视觉层，
	// 不碰用户数据/前台/真实指针，全部 Read(1)（2026-09-09 用户定：
	// Windows 浮层残留导致系统指针闪烁时需无摩擦止血）。
	"cursor": proto.LevelRead,

	"launch":    proto.LevelWrite,
	"navigate":  proto.LevelWrite,
	"bclick":    proto.LevelWrite,
	"btype":     proto.LevelWrite,
	"bend":      proto.LevelWrite,
	"click":     proto.LevelWrite,
	"dclick":    proto.LevelWrite,
	"rclick":    proto.LevelWrite,
	"type":      proto.LevelWrite,
	"key":       proto.LevelWrite,
	"hotkey":    proto.LevelWrite,
	"scroll":    proto.LevelWrite,
	"drag":      proto.LevelWrite,
	"move":      proto.LevelWrite,
	"set-value": proto.LevelWrite,
	"menu":      proto.LevelWrite,
	"set-frame": proto.LevelWrite,
	// front：前台激活应用（窃取用户前台焦点，用户可见接管）→ Danger(3)
	"front": proto.LevelDanger,

	// run：JS 脚本执行——内容可含任意动作无法静态分级，恒 Danger(3) 逐次
	// 审批（脚本全文随审批可见），对齐 shell 逃生舱语义。
	"run": proto.LevelDanger,
}

// cuaValueFlags 是 cua 带值 flag 表（子命令判定跳过其值；布尔 flag 不在列）。
// 未知 flag 由 host 侧原样透传驱动（不在本表）——子命令恒为首个非 flag 元素，
// 故不影响判定；未知 flag 在子命令之前时落入 Danger 兜底（保守方向）。
var cuaValueFlags = map[string]bool{
	"--pid": true, "--window": true, "--token": true,
	"--x": true, "--y": true, "--x1": true, "--y1": true, "--x2": true, "--y2": true,
	"--text": true, "--app": true, "--value": true, "--path": true,
	"--direction": true, "--amount": true, "--width": true, "--height": true,
	"--delivery": true, "--scope": true,
	"--url": true, "--query": true, "--ref": true, "--mode": true, "--route": true,
	"--grep": true, "--context": true, "--target": true, "--tab": true,
	"--code": true, "--file": true,
}

// jsonSubLevels 是 json 子命令分级（view=Read，修改类=Write——对齐 fs write/edit）。
var jsonSubLevels = map[string]int{
	"view":   proto.LevelRead,
	"set":    proto.LevelWrite,
	"del":    proto.LevelWrite,
	"append": proto.LevelWrite,
	"merge":  proto.LevelWrite,
}

// ExecRequired 返回 exec action 的 required level：
// 内建表 → git/browser 子命令 → Danger(3) 兜底（程序基线 + 未声明虚拟指令，§2.4）。
func ExecRequired(action string, argv []string) int {
	if lv, ok := execCoreLevels[action]; ok {
		return lv
	}
	switch action {
	case "git":
		return gitRequired(argv)
	case "browser":
		return browserRequired(argv)
	case "cua":
		return cuaRequired(argv)
	case "json":
		return jsonRequired(argv)
	}
	return proto.LevelDanger
}

// gitRequired 判定 git 子命令等级：跳过带值 flag（-C/-c 已知表），
// 取首个非 flag 且非 flag 值的元素（§5.5）。
func gitRequired(argv []string) int {
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if gitValueFlags[a] {
			i++ // 跳过 flag 值
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		if lv, ok := gitSubLevels[a]; ok {
			// checkout 的 pathspec 形态丢弃工作区未提交修改，不可恢复——
			// 与 reset 同级 Danger
			if a == "checkout" && checkoutPathspecLike(argv[i+1:]) {
				return proto.LevelDanger
			}
			return lv
		}
		return proto.LevelDanger // 未知子命令按 Danger 兜底
	}
	return proto.LevelDanger
}

// checkoutPathspecLike 判定 checkout 参数是否呈 pathspec 形态（丢弃工作区
// 未提交修改 → Danger）。两条触发路径：
//   - 显式 `--` 分隔符（checkout -- <path> / checkout <commit> -- <path>）；
//   - 无 `--` 但参数呈「不可能是合法 git refname」的形态——git check-ref-format
//     规定 refname 组件不能以 . 开头、不能以 / 结尾、不能含 \ : * ? [ 空格，
//     故此类参数出现即必为路径而非分支（`checkout .`/`checkout ./src` 等
//     经典丢弃修改写法）。
//
// 已知残余缺口：`checkout <纯文件名>`（如 checkout README.md）与分支名静态
// 不可区分（git 运行时先按分支解析、落空再按 pathspec），不提升——文档化缺口。
// 注意 `~`/`^` 刻意不在标记集内：`checkout HEAD~1`（detached，git 自身拒绝
// 覆盖未提交修改）保持 Write。
func checkoutPathspecLike(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return true
		}
		if strings.HasPrefix(a, "-") {
			continue // flag；-b/-B/--orphan 的值随后——其值为非法 refname 形态时误升 Danger 无害（git 同样拒绝）
		}
		if a == "." || a == ".." ||
			strings.HasPrefix(a, "./") || strings.HasPrefix(a, "../") ||
			strings.HasPrefix(a, "/") || strings.HasSuffix(a, "/") ||
			strings.ContainsAny(a, "\\:*?[ ") {
			return true
		}
	}
	return false
}

// browserRequired 判定 browser 子命令等级：读类 Read(1)，
// 其余核心页面交互统一 Write(2) 基线（§2.4：click 逐次确认会使浏览自动化不可用）。
func browserRequired(argv []string) int {
	sub := ""
	for _, a := range argv {
		if !strings.HasPrefix(a, "-") {
			sub = a
			break
		}
	}
	if lv, ok := browserSubLevels[sub]; ok {
		return lv
	}
	return proto.LevelWrite
}

// cuaRequired 判定 cua 子命令等级：
//  1. 全参数扫描 --delivery foreground / --scope desktop → Danger(3)（用户可见接管）；
//  2. 取首个非 flag 且非 flag 值的子命令查表（读类 Read，交互 Write；cursor 全 Read）；
//  3. clipboard 嵌套子命令：read=Read，write=Write；
//  4. bprepare 嵌套：--isolated=Write（驱动自持隔离 profile），
//     缺省 existing_profile=Danger（开启用户真实浏览器的远程调试，逐次审批）；
//  5. 未知子命令 Danger 兑底（未知动作保守）。
func cuaRequired(argv []string) int {
	danger := false
	sub, clipboardSub := "", ""
	isolated := false
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--isolated" {
			isolated = true
		}
		// --delivery / --delivery-mode（透传写法）foreground 均提级 Danger：
		// 两个写法都必须命中，避免透传绕过前台接管的逐次审批。
		if (a == "--delivery" || a == "--delivery-mode") && i+1 < len(argv) && argv[i+1] == "foreground" {
			danger = true
		}
		// --scope desktop = 真实物理指针（移动/点击用户鼠标，用户可见接管）→ Danger
		if a == "--scope" && i+1 < len(argv) && argv[i+1] == "desktop" {
			danger = true
		}
		if cuaValueFlags[a] {
			i++ // 跳过 flag 值
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		if sub == "" {
			sub = a
			continue
		}
		if sub == "clipboard" && clipboardSub == "" {
			clipboardSub = a
		}
	}
	if danger {
		return proto.LevelDanger
	}
	if sub == "clipboard" {
		if clipboardSub == "read" {
			return proto.LevelRead
		}
		return proto.LevelWrite
	}
	if sub == "bprepare" {
		if isolated {
			return proto.LevelWrite
		}
		return proto.LevelDanger
	}
	if lv, ok := cuaSubLevels[sub]; ok {
		return lv
	}
	return proto.LevelDanger
}

// jsonRequired 判定 json 子命令等级：首个非 flag 参数（json 无带值 flag）。
// 未知子命令按 Write 兜底（修改类，保守）。
func jsonRequired(argv []string) int {
	sub := ""
	for _, a := range argv {
		if !strings.HasPrefix(a, "-") {
			sub = a
			break
		}
	}
	if lv, ok := jsonSubLevels[sub]; ok {
		return lv
	}
	return proto.LevelWrite
}
