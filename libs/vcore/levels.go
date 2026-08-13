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

	"push":     proto.LevelDanger, // 外发远端
	"checkout": proto.LevelDanger, // 可丢弃本地修改
	"reset":    proto.LevelDanger, // 可丢弃本地修改
}

// gitValueFlags 是 git 带值 flag 已知表（§5.5：子命令判定先跳过带值 flag）。
var gitValueFlags = map[string]bool{"-C": true, "-c": true}

// browserSubLevels 是 browser 子命令分级（§2.4）。
// host 端 eval 单独提升至 Danger（读已登录站点 DOM/cookie），由 caps 声明覆盖。
var browserSubLevels = map[string]int{
	"snapshot":   proto.LevelRead,
	"read":       proto.LevelRead,
	"get":        proto.LevelRead,
	"screenshot": proto.LevelRead,
	"network":    proto.LevelRead,

	"upload": proto.LevelDanger, // 文件外发
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
			return lv
		}
		return proto.LevelDanger // 未知子命令按 Danger 兜底
	}
	return proto.LevelDanger
}

// browserRequired 判定 browser 子命令等级：读类 Read(1)，upload Danger(3)，
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
