package vcore

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// ---- ls（§5.4）----

// ls [-l] [-a] [-t] [-h] [path]：path 缺省 = workdir；目录每行一个条目名
// （目录加 / 后缀）；默认隐藏点文件，-a 显示；-l 追加 size/mtime_unix 列，
// -h 人类可读大小（1024 进制，仅 -l 时生效，对齐 GNU ls -h）；
// -t 按 mtime 降序（同 mtime 按名称升序，稳定），默认名称 UTF-8 字节序；
// 单文件输出文件名（Attrs path_kind=file）。help 统一 --help（-h 为 human flag）。
func cmdLs(ctx context.Context, env *Env, argv []string) (*Result, error) {
	pa, err := parseArgv("ls", argvSpec{
		bools:  map[string]bool{"-l": true, "-a": true, "-t": true, "-h": true},
		minPos: 0, maxPos: 1,
	}, argv)
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
	if err := env.CheckPath("ls", abs); err != nil {
		return nil, err
	}
	long, all, byTime, human := pa.bools["-l"], pa.bools["-a"], pa.bools["-t"], pa.bools["-h"]

	info, err := env.VFS.Stat(abs)
	if err != nil {
		return nil, execErr("ls", "%s", err)
	}
	r := newResult("ls", abs)
	if !info.IsDir() {
		// 单文件：输出文件名（对齐真实 ls）
		r.Content = formatLsLine(info.Name(), false, info.Size(), info.ModTime().Unix(), long, human)
		r.Attrs["path_kind"] = "file"
		r.set("rows", 1)
		r.set("truncated", false)
		return r, nil
	}
	entries, err := env.VFS.ReadDir(abs)
	if err != nil {
		return nil, execErr("ls", "%s", err)
	}
	// 默认隐藏点文件与 "." 开头条目（对齐真实 ls），-a 显示全部
	visible := entries[:0]
	for _, e := range entries {
		if !all && strings.HasPrefix(e.Name(), ".") {
			continue
		}
		visible = append(visible, e)
	}
	if len(visible) == 0 {
		r.Content = fmt.Sprintf("empty directory: %s", abs)
		r.Attrs["path_kind"] = "directory"
		r.set("rows", 0)
		r.set("truncated", false)
		return r, nil
	}
	if byTime {
		// mtime 降序（对齐真实 ls -t）；同 mtime 按名称升序（稳定，§5.4）
		sort.SliceStable(visible, func(i, j int) bool {
			fi, _ := visible[i].Info()
			fj, _ := visible[j].Info()
			var ti, tj int64
			if fi != nil {
				ti = fi.ModTime().UnixNano()
			}
			if fj != nil {
				tj = fj.ModTime().UnixNano()
			}
			if ti != tj {
				return ti > tj
			}
			return visible[i].Name() < visible[j].Name()
		})
	} else {
		// UTF-8 字节序排序（禁止 locale 相关排序，§5.4）
		sort.SliceStable(visible, func(i, j int) bool { return visible[i].Name() < visible[j].Name() })
	}
	lines := make([]string, 0, len(visible))
	for _, e := range visible {
		var size, mt int64
		if long {
			if fi, _ := e.Info(); fi != nil {
				size, mt = fi.Size(), fi.ModTime().Unix()
			}
		}
		lines = append(lines, formatLsLine(e.Name(), e.IsDir(), size, mt, long, human))
	}
	r.Content = strings.Join(lines, "\n")
	r.Attrs["path_kind"] = "directory"
	r.set("rows", len(lines))
	r.set("truncated", false)
	return r, nil
}

// formatLsLine 格式化一行输出：name（目录加 / 后缀），long 时追加 size/mtime 列；
// human 时 size 用人类可读格式（GNU ls -h 对齐）。
func formatLsLine(name string, dir bool, size, mtimeUnix int64, long, human bool) string {
	if dir {
		name += "/"
	}
	if !long {
		return name
	}
	if human {
		return fmt.Sprintf("%s\t%s\t%d", name, humanSize(size), mtimeUnix)
	}
	return fmt.Sprintf("%s\t%d\t%d", name, size, mtimeUnix)
}

// humanSize 对齐 GNU ls -h：1024 进制单字母后缀（B/K/M/G/T/P/E），
// 非整单位保留 1 位小数（36B、1.5K、2.3M）。
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%c", float64(n)/float64(div), "KMGTPE"[exp])
}
