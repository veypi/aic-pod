package hostfs

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"syscall"

	fsp "github.com/veypi/aic-pod/protocol/fs"
	hosts "github.com/veypi/aic-pod/protocol/fs"
)

type findArgs struct {
	Path  fsp.Path `json:"path"`
	Glob  string   `json:"glob,omitempty"`
	Depth int      `json:"depth,omitempty"`
	Limit int      `json:"limit,omitempty"`
}
type moveArgs struct {
	Src       fsp.Path      `json:"src"`
	Dst       fsp.Path      `json:"dst"`
	IfVersion string        `json:"if_version"`
	Condition fsp.Condition `json:"condition"`
}
type walkEntry struct {
	path fsp.Path
	info fs.FileInfo
}

var stopWalk = errors.New("walk budget exhausted")

func child(p fsp.Path, name string) fsp.Path {
	return fsp.Path{RootID: p.RootID, Segments: append(append([]string{}, p.Segments...), name)}
}
func (f *FS) info(ctx context.Context, call Call, p fsp.Path, write bool) (fs.FileInfo, error) {
	r, abs, err := f.check(ctx, call, p, write)
	if err != nil {
		return nil, err
	}
	h, name, err := parent(r, abs)
	if err != nil {
		return nil, err
	}
	defer h.Close()
	return h.Lstat(name)
}

// walk 遍历 p 子树至 depth 层（含 p 自身）。skipDenied 忽略无权限条目；
// skipHidden 跳过隐藏条目（点开头或 Windows 隐藏属性，不访问不递归——find 用，
// 与 list 的 hidden=false 缺省、rg 与 cloud/page 搜索的缺省口径一致）。
func (f *FS) walk(ctx context.Context, call Call, p fsp.Path, depth int, write, skipDenied, skipHidden bool, visit func(walkEntry) error) error {
	count := 0
	var walk func(fsp.Path, int) error
	walk = func(p fsp.Path, level int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		count++
		if count > f.cfg.MaxDirectoryEntries {
			return stopWalk
		}
		info, err := f.info(ctx, call, p, write)
		if err != nil {
			// 读侧遍历（find 等）：起点的 permission_denied 与子项的一切取信息失败
			// （Windows 受保护目录/联结、独占文件等）都跳过——单条不可访问不应中断
			// 整棵搜索；写侧（remove/copy/move）保持严格，起点其余错误照常上抛。
			if skipDenied && (level > 0 || hosts.AsFault(fault(err)).Code == "permission_denied") {
				return nil
			}
			return err
		}
		if err = visit(walkEntry{p, info}); err != nil {
			return err
		}
		if !info.IsDir() || level == depth {
			return nil
		}
		r, abs, err := f.check(ctx, call, p, write)
		if err != nil {
			return err
		}
		h, name, err := parent(r, abs)
		if err != nil {
			return err
		}
		dir, err := h.OpenRoot(name)
		h.Close()
		if err != nil {
			return err
		}
		defer dir.Close()
		opened, err := dir.Stat(".")
		if err != nil || !os.SameFile(info, opened) {
			return hosts.Fail("source_changed", "Directory changed during enumeration")
		}
		file, err := dir.Open(".")
		if err != nil {
			return err
		}
		entries, err := file.ReadDir(f.cfg.MaxDirectoryEntries + 1)
		file.Close()
		if err != nil && err != io.EOF {
			return err
		}
		if len(entries) > f.cfg.MaxDirectoryEntries {
			return stopWalk
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, e := range entries {
			if skipHidden && (strings.HasPrefix(e.Name(), ".") || entryHidden(e)) {
				continue
			}
			if err := walk(child(p, e.Name()), level+1); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(p, 0)
}
func (f *FS) find(ctx context.Context, call Call, p findArgs) (any, error) {
	if p.Limit == 0 {
		p.Limit = 100
	}
	if p.Depth == 0 {
		p.Depth = 64
	}
	if p.Glob == "" {
		p.Glob = "*"
	}
	out := make([]fsp.Entry, 0, p.Limit)
	// 隐藏项不访问不递归（与 ls/rg 缺省、cloud/page 搜索口径一致）。
	err := f.walk(ctx, call, p.Path, p.Depth, false, true, true, func(e walkEntry) error {
		if len(e.path.Segments) == len(p.Path.Segments) {
			return nil
		}
		matches, _ := path.Match(p.Glob, e.info.Name())
		if matches {
			out = append(out, entry(e.path, e.info))
			if len(out) >= p.Limit {
				return stopWalk
			}
		}
		return nil
	})
	truncated := errors.Is(err, stopWalk)
	if err != nil && !truncated {
		return nil, err
	}
	return map[string]any{"entries": out, "truncated": truncated}, nil
}
func partial(err error, completed int) error {
	result := hosts.AsFault(fault(err))
	if errors.Is(err, context.Canceled) {
		result = hosts.Fail("cancelled", "Filesystem operation cancelled")
	} else if errors.Is(err, context.DeadlineExceeded) {
		result = hosts.Fail("deadline_exceeded", "Filesystem operation deadline elapsed")
	}
	if completed > 0 {
		result.Effect = "partial"
	}
	result.Details = map[string]any{"completed": completed}
	return result
}
func (f *FS) mkdirParents(ctx context.Context, call Call, p mkdirArgs) (any, error) {
	if len(p.Path.Segments) == 0 {
		return nil, hosts.Fail("permission_denied", "Cannot create a root")
	}
	// Start at the requested path and stop at the nearest existing directory.
	// An allowed /Users/me/project must not require write permission on /Users.
	// 探测不做策略门控：已存在的祖先不需要写授权（谁被创建才查谁，与
	// edit/mkdir/remove 同口径）；写预检只覆盖将创建的层级——先检后建，
	// 拒绝时零副作用；创建时 f.mkdir 仍逐级复查。
	var missing []fsp.Path
	current := p.Path
	var value any
	for {
		before, err := f.probe(current)
		if err == nil {
			if !before.IsDir() {
				return nil, hosts.Fail("already_exists", "Parent is not a directory")
			}
			if len(missing) == 0 && !p.ExistOK {
				return nil, fs.ErrExist
			}
			value = entry(current, before)
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		if len(current.Segments) == 0 {
			return nil, err
		}
		missing = append(missing, current)
		current = fsp.Path{RootID: current.RootID, Segments: current.Segments[:len(current.Segments)-1]}
	}
	for _, m := range missing {
		if _, _, err := f.check(ctx, call, m, true); err != nil {
			return nil, err
		}
	}
	created := 0
	for i := len(missing) - 1; i >= 0; i-- {
		var err error
		value, err = f.mkdir(ctx, call, mkdirArgs{Path: missing[i]})
		if err != nil {
			return nil, partial(err, created)
		}
		created++
	}
	return value, nil
}
func (f *FS) removeTree(ctx context.Context, call Call, p removeArgs) (any, error) {
	if len(p.Path.Segments) == 0 {
		return nil, hosts.Fail("permission_denied", "Cannot remove a root")
	}
	info, err := f.info(ctx, call, p.Path, true)
	if errors.Is(err, fs.ErrNotExist) && p.MissingOK {
		return map[string]bool{"removed": false}, nil
	}
	if err != nil {
		return nil, err
	}
	if version(info) != p.IfVersion {
		return nil, hosts.Fail("version_conflict", "Removal source changed")
	}
	var planned []walkEntry
	err = f.walk(ctx, call, p.Path, 256, true, false, false, func(e walkEntry) error { planned = append(planned, e); return nil })
	if errors.Is(err, stopWalk) {
		return nil, hosts.Fail("overloaded", "Removal exceeds entry budget")
	}
	if err != nil {
		return nil, err
	}
	completed := 0
	for i := len(planned) - 1; i >= 0; i-- {
		e := planned[i]
		current, err := f.info(ctx, call, e.path, true)
		if err != nil {
			return nil, partial(err, completed)
		}
		if !os.SameFile(current, e.info) || (!current.IsDir() && version(current) != version(e.info)) {
			return nil, partial(hosts.Fail("version_conflict", "Removal source changed"), completed)
		}
		_, err = f.remove(ctx, call, removeArgs{Path: e.path, IfVersion: version(current)})
		if err != nil {
			return nil, partial(err, completed)
		}
		completed++
	}
	return map[string]any{"removed": true, "path": p.Path, "completed": completed}, nil
}
func (f *FS) move(ctx context.Context, call Call, p moveArgs) (any, error) {
	if len(p.Src.Segments) == 0 || len(p.Dst.Segments) == 0 {
		return nil, hosts.Fail("permission_denied", "Cannot move or replace a root")
	}
	sr, sourcePath, err := f.check(ctx, call, p.Src, true)
	if err != nil {
		return nil, err
	}
	dr, targetPath, err := f.check(ctx, call, p.Dst, true)
	if err != nil {
		return nil, err
	}
	sh, sn, err := parent(sr, sourcePath)
	if err != nil {
		return nil, err
	}
	defer sh.Close()
	dh, dn, err := parent(dr, targetPath)
	if err != nil {
		return nil, err
	}
	defer dh.Close()
	info, err := sh.Lstat(sn)
	if err != nil {
		return nil, err
	}
	if version(info) != p.IfVersion {
		return nil, hosts.Fail("version_conflict", "Move source changed")
	}
	// Moving a directory also moves every descendant across policy boundaries.
	if err = f.walk(ctx, call, p.Src, 256, true, false, false, func(e walkEntry) error {
		dst := fsp.Path{RootID: p.Dst.RootID, Segments: append(append([]string{}, p.Dst.Segments...), e.path.Segments[len(p.Src.Segments):]...)}
		_, _, err := f.check(ctx, call, dst, true)
		return err
	}); err != nil {
		if errors.Is(err, stopWalk) {
			return nil, hosts.Fail("overloaded", "Move exceeds entry budget")
		}
		return nil, err
	}
	current, err := sh.Lstat(sn)
	if err != nil {
		return nil, err
	}
	if version(current) != p.IfVersion {
		return nil, hosts.Fail("version_conflict", "Move source changed")
	}
	target, err := dh.Lstat(dn)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	v := ""
	if exists {
		v = version(target)
	}
	if err = p.Condition.Check(v, exists); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if _, _, err = f.check(ctx, call, p.Src, true); err != nil {
		return nil, err
	}
	if _, _, err = f.check(ctx, call, p.Dst, true); err != nil {
		return nil, err
	}
	if err = f.cfg.Check(ctx, call, sourcePath, true); err != nil {
		return nil, err
	}
	if err = f.cfg.Check(ctx, call, targetPath, true); err != nil {
		return nil, err
	}
	if p.Condition.Absent {
		from, openErr := sh.Open(".")
		if openErr != nil {
			return nil, openErr
		}
		defer from.Close()
		to, openErr := dh.Open(".")
		if openErr != nil {
			return nil, openErr
		}
		defer to.Close()
		err = renameNoReplace(int(from.Fd()), sn, int(to.Fd()), dn)
	} else {
		from, e := sh.Open(".")
		if e != nil {
			return nil, e
		}
		defer from.Close()
		to, e := dh.Open(".")
		if e != nil {
			return nil, e
		}
		defer to.Close()
		err = renameReplace(int(from.Fd()), sn, int(to.Fd()), dn)
	}
	if errors.Is(err, syscall.EXDEV) {
		return nil, hosts.Fail("cross_device", "Move requires the same filesystem")
	}
	if err != nil {
		return nil, err
	}
	moved, err := dh.Lstat(dn)
	if err != nil {
		return nil, partial(err, 1)
	}
	return entry(p.Dst, moved), nil
}
