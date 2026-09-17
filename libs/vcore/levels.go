package vcore

import (
	"encoding/json"
	"strings"

	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/aic-pod/protocol/ui"
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
			if a == "branch" && len(argv[i+1:]) > 0 {
				return branchRequired(argv[i+1:])
			}
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

// Browser/CUA classification uses exactly the parser and schema used by executors.
func browserRequired(argv []string) int { return ui.Required("browser", argv) }
func cuaRequired(argv []string) int     { return ui.Required("cua", argv) }

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

// Listing branches is a read; creating a branch writes refs, and destructive
// branch flags require the danger level before the command reaches an executor.
func branchRequired(args []string) int {
	list := false
	level := proto.LevelRead
	for _, a := range args {
		switch a {
		case "-d", "-D", "-m", "-M", "-c", "-C", "-f", "--delete", "--move", "--copy", "--force", "--unset-upstream":
			return proto.LevelDanger
		case "--list":
			list = true
		case "-a", "-r", "-v", "-vv", "--all", "--remotes", "--verbose", "--show-current":
		default:
			if strings.HasPrefix(a, "-") {
				return proto.LevelDanger
			}
			level = proto.LevelWrite
		}
	}
	if list {
		return proto.LevelRead
	}
	return level
}
