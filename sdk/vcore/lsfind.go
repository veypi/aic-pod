package vcore

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// skipDirs 是 find/grep -r 不进入的目录（§5.4：node_modules/vendor 与 . 开头目录）。
var skipDirs = map[string]bool{"node_modules": true, "vendor": true}

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

// ---- find（§5.4）----

// find <path> [-name <glob>]：递归列出，每行 {完整路径}{目录加/}\t{size}\t{mtime_unix}，
// mtime 倒序，默认上限 100 条（查 limit+1 判定截断），跳过表见 skipDirs。
func cmdFind(ctx context.Context, env *Env, argv []string) (*Result, error) {
	pa, err := parseArgv("find", argvSpec{values: map[string]bool{"-name": true}, minPos: 1, maxPos: 1}, argv)
	if err != nil {
		return nil, err
	}
	abs, err := env.Resolve(pa.pos[0])
	if err != nil {
		return nil, execErr("find", "%s", err)
	}
	glob := pa.values["-name"]
	const limit = 100

	type entry struct {
		path  string
		dir   bool
		size  int64
		mtime int64
	}
	var found []entry
	rootInfo, err := env.VFS.Stat(abs)
	if err != nil {
		return nil, execErr("find", "%s", err)
	}
	var walk func(dir string) error
	walk = func(dir string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := env.VFS.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			name := e.Name()
			// 跳过 node_modules/vendor 与 . 开头目录（§5.4）；点文件不跳过
			if e.IsDir() && (skipDirs[name] || strings.HasPrefix(name, ".")) {
				continue
			}
			full := dir + "/" + name
			if glob != "" && !globMatch(glob, name) {
				if e.IsDir() {
					if err := walk(full); err != nil {
						return err
					}
				}
				continue
			}
			fi, _ := e.Info()
			var size, mt int64
			if fi != nil {
				size, mt = fi.Size(), fi.ModTime().Unix()
			}
			found = append(found, entry{full, e.IsDir(), size, mt})
			if e.IsDir() {
				if err := walk(full); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if !rootInfo.IsDir() {
		if glob == "" || globMatch(glob, rootInfo.Name()) {
			found = append(found, entry{abs, false, rootInfo.Size(), rootInfo.ModTime().Unix()})
		}
	} else if err := walk(abs); err != nil {
		return nil, execErr("find", "%s", err)
	}

	// mtime 倒序；并列按路径字节序（向量锁定）
	sort.Slice(found, func(i, j int) bool {
		if found[i].mtime != found[j].mtime {
			return found[i].mtime > found[j].mtime
		}
		return found[i].path < found[j].path
	})
	truncated := len(found) > limit
	if truncated {
		found = found[:limit]
	}

	r := newResult("find", abs)
	if len(found) == 0 {
		r.Content = fmt.Sprintf("no files matched glob %q in %s", glob, abs)
		r.set("rows", 0)
		r.set("truncated", false)
		return r, nil
	}
	var b strings.Builder
	rows := 0
	for _, f := range found {
		label := f.path
		if f.dir {
			label += "/"
		}
		row := fmt.Sprintf("%s\t%d\t%d\n", label, f.size, f.mtime)
		// 512KB 字节预算只留完整行（§2.5）
		if rows > 0 && b.Len()+len(row) > MaxContentBytes {
			truncated = true
			break
		}
		b.WriteString(row)
		rows++
	}
	r.Content = strings.TrimRight(b.String(), "\n")
	r.set("rows", rows)
	r.set("truncated", truncated)
	return r, nil
}
