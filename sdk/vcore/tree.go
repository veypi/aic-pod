package vcore

import (
	"context"
	"encoding/json"
	"path"
	"sort"
	"strconv"
)

// ---- tree（§5.4，机器消费 JSON 输出，§2.2 显式声明）----
//
// tree [path] [--depth N]：递归结构化目录树，一次取回有界子树。
// 主要服务前端文件选择器（避免逐级 ls 的雪崩放大）；agent 亦可用（结构化 ls）。
//
//   - 默认 depth=3，上限 treeMaxDepth；节点上限 treeMaxNodes，超限 truncated=true；
//   - 递归跳过 treeSkipDirs 与点开头目录（仅不 descend，仍作为叶条目出现——
//     与前端 tree_cache 的跳过集对齐，避免 .git/node_modules 爆炸）；点文件不跳过；
//   - 输出（目录）：{"dir":true,"cwd":<展开后绝对路径>,"truncated":bool,"items":[entry...]}；
//     输出（文件）：{"dir":false,"name","size","mod_time","mime"}；
//   - entry：{"name","dir","size","mod_time","mime","items"?}；子目录已展开时带 items
//     （空目录为 []），未展开（深度耗尽/被跳过）时省略 items。字段名对齐前端 tree_cache。
const (
	treeDefaultDepth = 3
	treeMaxDepth     = 5
	treeMaxNodes     = 2000
)

// treeSkipDirs 是 tree 不 descend 的目录名（与前端 tree_cache SKIP_DIRS 对齐）。
var treeSkipDirs = map[string]bool{
	"node_modules": true, "vendor": true, "__pycache__": true,
	"bower_components": true, "dist": true, "build": true, "target": true,
	".next": true, ".nuxt": true, "coverage": true, ".turbo": true, ".output": true,
}

type treeEntry struct {
	Name    string       `json:"name"`
	Dir     bool         `json:"dir"`
	Size    int64        `json:"size"`
	ModTime int64        `json:"mod_time"`
	Mime    string       `json:"mime"`
	Items   *[]treeEntry `json:"items,omitempty"`
}

type treeState struct {
	count     int
	truncated bool
}

func cmdTree(ctx context.Context, env *Env, argv []string) (*Result, error) {
	pa, err := parseArgv("tree", argvSpec{
		values: map[string]bool{"--depth": true},
		minPos: 0, maxPos: 1,
	}, argv)
	if err != nil {
		return nil, err
	}
	depth := treeDefaultDepth
	if v := pa.values["--depth"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, execErr("tree", "--depth must be >= 1, got %s", v)
		}
		depth = n
	}
	if depth > treeMaxDepth {
		depth = treeMaxDepth
	}
	target := env.Workdir
	if len(pa.pos) == 1 {
		target = pa.pos[0]
	}
	abs, err := env.Resolve(target)
	if err != nil {
		return nil, execErr("tree", "%s", err)
	}
	if err := env.CheckPath("tree", abs); err != nil {
		return nil, err
	}
	info, err := env.VFS.Stat(abs)
	if err != nil {
		return nil, execErr("tree", "%s", err)
	}

	r := newResult("tree", abs)
	// 单文件：直接返回文件条目（前端 tree_cache 走 dir===false 分支）
	if !info.IsDir() {
		e := treeEntry{Name: path.Base(abs), Dir: false, Size: info.Size(), ModTime: info.ModTime().Unix()}
		b, _ := json.Marshal(e)
		r.Content = string(b)
		r.set("rows", 1)
		r.set("truncated", false)
		return r, nil
	}

	st := &treeState{}
	items := buildTreeDir(ctx, env, abs, depth, st)
	sortTreeEntries(items)
	out := map[string]any{"dir": true, "cwd": abs, "truncated": st.truncated, "items": items}
	b, _ := json.Marshal(out)
	r.Content = string(b)
	r.set("rows", st.count)
	r.set("truncated", st.truncated)
	return r, nil
}

// buildTreeDir 收集 dir 的直接子项；remain 为还需展开的层数（>1 时对未跳过子目录递归）。
func buildTreeDir(ctx context.Context, env *Env, dir string, remain int, st *treeState) []treeEntry {
	if st.truncated {
		return nil
	}
	entries, err := env.VFS.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]treeEntry, 0, len(entries))
	for _, e := range entries {
		if st.count >= treeMaxNodes {
			st.truncated = true
			break
		}
		if err := ctx.Err(); err != nil {
			break
		}
		name := e.Name()
		full := dir + "/" + name
		var size, mt int64
		if fi, _ := e.Info(); fi != nil {
			size, mt = fi.Size(), fi.ModTime().Unix()
		}
		ent := treeEntry{Name: name, Dir: e.IsDir(), Size: size, ModTime: mt}
		st.count++
		if e.IsDir() {
			skipDescend := name[0] == '.' || treeSkipDirs[name]
			if remain > 1 && !skipDescend && !st.truncated {
				sub := buildTreeDir(ctx, env, full, remain-1, st)
				ent.Items = &sub // 已展开（空目录为 []），与"未展开省略 items"区分
			}
		}
		out = append(out, ent)
	}
	return out
}

func sortTreeEntries(items []treeEntry) {
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	for _, it := range items {
		if it.Items != nil {
			sortTreeEntries(*it.Items)
		}
	}
}
