package vcore

import (
	"context"
	"encoding/json"
	"path"
	"sort"
)

// ---- ls（§4.5：目录列举，depth>1 即递归树——吸收原 tree 指令）----
//
// fs ls 参数：{path?, depth=1(≤5), all=false, sort="name"|"time"}。
// 输出恒为 JSON（机器消费，§2.2 显式声明）：
//   - 目录：{"dir":true,"cwd":<展开后绝对路径>,"truncated":bool,"items":[entry...]}
//   - 文件：{"dir":false,"name","size","mod_time"}
//   - entry：{"name","dir","size","mod_time","items"?}；子目录已展开时带 items
//     （空目录为 []），未展开（深度耗尽/被跳过）时省略 items。字段名对齐前端 tree_cache。
//
// 规则：隐藏项（点开头）默认完全跳过（不显示不递归），all=true 收录；
// lsSkipDirs（node_modules 等大目录）不 descend，仍作为叶条目出现；
// 节点上限 lsMaxNodes，超限 truncated=true。
const (
	lsDefaultDepth = 1
	lsMaxDepth     = 5
	lsMaxNodes     = 2000
)

// lsSkipDirs 是递归时不 descend 的目录名（与前端 tree_cache 的跳过集对齐）。
var lsSkipDirs = map[string]bool{
	"node_modules": true, "vendor": true, "__pycache__": true,
	"bower_components": true, "dist": true, "build": true, "target": true,
	".next": true, ".nuxt": true, "coverage": true, ".turbo": true, ".output": true,
}

type lsEntry struct {
	Name    string    `json:"name"`
	Dir     bool      `json:"dir"`
	Size    int64     `json:"size"`
	ModTime int64     `json:"mod_time"`
	Items   *[]lsEntry `json:"items,omitempty"`
}

type lsState struct {
	count     int
	truncated bool
}

func fsLs(ctx context.Context, env *Env, p *fsParams) (*Result, error) {
	depth := lsDefaultDepth
	if p.Depth != nil {
		if *p.Depth < 1 {
			return nil, fsErr("ls", "depth must be >= 1, got %d", *p.Depth)
		}
		depth = *p.Depth
	}
	if depth > lsMaxDepth {
		depth = lsMaxDepth
	}
	byTime := false
	switch p.Sort {
	case "", "name":
	case "time":
		byTime = true
	default:
		return nil, fsErr("ls", "sort must be \"name\" or \"time\", got %q", p.Sort)
	}

	target := env.Workdir
	if p.Path != "" {
		target = p.Path
	}
	abs, err := env.Resolve(target)
	if err != nil {
		return nil, fsErr("ls", "%s", err)
	}
	if err := env.CheckPath("ls", abs); err != nil {
		return nil, err
	}
	info, err := env.VFS.Stat(abs)
	if err != nil {
		return nil, fsErr("ls", "%s", err)
	}

	r := newResult("ls", abs)
	// 单文件：直接返回文件条目（前端 tree_cache 走 dir===false 分支）
	if !info.IsDir() {
		e := lsEntry{Name: path.Base(abs), Dir: false, Size: info.Size(), ModTime: info.ModTime().Unix()}
		b, _ := json.Marshal(e)
		r.Content = string(b)
		r.set("rows", 1)
		r.set("truncated", false)
		return r, nil
	}

	st := &lsState{}
	items := buildLsDir(ctx, env, abs, depth, p.All, st)
	sortLsEntries(items, byTime)
	out := map[string]any{"dir": true, "cwd": abs, "truncated": st.truncated, "items": items}
	b, _ := json.Marshal(out)
	r.Content = string(b)
	r.set("rows", st.count)
	r.set("truncated", st.truncated)
	return r, nil
}

// buildLsDir 收集 dir 的直接子项；remain 为还需展开的层数（>1 时对未跳过子目录递归）。
func buildLsDir(ctx context.Context, env *Env, dir string, remain int, all bool, st *lsState) []lsEntry {
	if st.truncated {
		return nil
	}
	entries, err := env.VFS.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]lsEntry, 0, len(entries))
	for _, e := range entries {
		if st.count >= lsMaxNodes {
			st.truncated = true
			break
		}
		if err := ctx.Err(); err != nil {
			break
		}
		name := e.Name()
		// 隐藏项（点开头）默认完全跳过：不显示不递归；all=true 收录
		if !all && name[0] == '.' {
			continue
		}
		full := dir + "/" + name
		var size, mt int64
		if fi, _ := e.Info(); fi != nil {
			size, mt = fi.Size(), fi.ModTime().Unix()
		}
		ent := lsEntry{Name: name, Dir: e.IsDir(), Size: size, ModTime: mt}
		st.count++
		if e.IsDir() {
			skipDescend := lsSkipDirs[name]
			if remain > 1 && !skipDescend && !st.truncated {
				sub := buildLsDir(ctx, env, full, remain-1, all, st)
				ent.Items = &sub // 已展开（空目录为 []），与"未展开省略 items"区分
			}
		}
		out = append(out, ent)
	}
	return out
}

// sortLsEntries 逐级排序：name = UTF-8 字节序（禁止 locale 相关排序）；
// time = mtime 降序（同值按名称升序，稳定）。
func sortLsEntries(items []lsEntry, byTime bool) {
	if byTime {
		sort.SliceStable(items, func(i, j int) bool {
			if items[i].ModTime != items[j].ModTime {
				return items[i].ModTime > items[j].ModTime
			}
			return items[i].Name < items[j].Name
		})
	} else {
		sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	}
	for _, it := range items {
		if it.Items != nil {
			sortLsEntries(*it.Items, byTime)
		}
	}
}
