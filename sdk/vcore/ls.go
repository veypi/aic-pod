package vcore

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// ---- ls（§5.4）----

// ls [-la] [path]：path 缺省 = workdir；目录每行一个条目名（目录加 / 后缀，
// UTF-8 字节序排序）；-la 追加 size/mtime_unix 列；单文件 Content 空 + path_kind=file。
func cmdLs(ctx context.Context, env *Env, argv []string) (*Result, error) {
	pa, err := parseArgv("ls", argvSpec{bools: map[string]bool{"-la": true}, minPos: 0, maxPos: 1}, argv)
	if err != nil {
		return nil, err
	}
	target := env.Workdir
	if len(pa.pos) == 1 {
		target = pa.pos[0]
	}
	abs, err := env.Resolve(target)
	if err != nil {
		return nil, execErr("ls", "%s", err)
	}
	long := pa.bools["-la"]

	info, err := env.VFS.Stat(abs)
	if err != nil {
		return nil, execErr("ls", "%s", err)
	}
	r := newResult("ls", abs)
	if !info.IsDir() {
		// 单文件：Content 空（§5.4 豁免，-la 同）
		r.Attrs["path_kind"] = "file"
		r.set("rows", 0)
		r.set("truncated", false)
		return r, nil
	}
	entries, err := env.VFS.ReadDir(abs)
	if err != nil {
		return nil, execErr("ls", "%s", err)
	}
	if len(entries) == 0 {
		r.Content = fmt.Sprintf("empty directory: %s", abs)
		r.Attrs["path_kind"] = "directory"
		r.set("rows", 0)
		r.set("truncated", false)
		return r, nil
	}
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		label := e.Name()
		if e.IsDir() {
			label += "/"
		}
		if long {
			fi, _ := e.Info()
			var size, mt int64
			if fi != nil {
				size, mt = fi.Size(), fi.ModTime().Unix()
			}
			label = fmt.Sprintf("%s\t%d\t%d", label, size, mt)
		}
		lines = append(lines, label)
	}
	// UTF-8 字节序排序（禁止 locale 相关排序，§5.4）
	sort.Strings(lines)
	r.Content = strings.Join(lines, "\n")
	r.Attrs["path_kind"] = "directory"
	r.set("rows", len(lines))
	r.set("truncated", false)
	return r, nil
}
