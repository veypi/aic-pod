package vcore

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/vigo/contrib/ufs"
)

func dirOf(p string) string { return path.Dir(p) }

// checkRootProtect 根目录硬保护（§5.4：ToolDeniedError，不可审批绕过）。
func checkRootProtect(env *Env, cmd, p string) error {
	for _, root := range env.ProtectRoots {
		if p == root {
			verb := "remove"
			if cmd == "mv" {
				verb = "move"
			}
			return &proto.DeniedError{Reason: fmt.Sprintf("%s: cannot %s root directory %s", cmd, verb, p)}
		}
	}
	return nil
}

// ---- rm（§4.7）----

// fsRm 实现 rm：{path, recursive}。文件/空目录直接删；非空目录需 recursive=true。
// recursive 删非空目录的权限动态提升（Danger）在 FSRequiredIn（§2.4）。
func fsRm(ctx context.Context, env *Env, p *fsParams) (*Result, error) {
	if p.Path == "" {
		return nil, fsErr("rm", "path is required")
	}
	abs, err := env.Resolve(p.Path)
	if err != nil {
		return nil, fsErr("rm", "%s", err)
	}
	if err := env.CheckPath("rm", abs); err != nil {
		return nil, err
	}
	if err := checkRootProtect(env, "rm", abs); err != nil {
		return nil, err
	}
	info, err := env.VFS.Stat(abs)
	if err != nil {
		return nil, fsErr("rm", "%s", err)
	}
	count := 0
	if info.IsDir() {
		entries, _ := env.VFS.ReadDir(abs)
		if len(entries) > 0 && !p.Recursive {
			return nil, fsErr("rm", "%s is a non-empty directory (set recursive=true)", abs)
		}
		if len(entries) > 0 {
			count = countEntries(env.VFS, abs)
		}
	}
	if err := env.VFS.RemoveAll(abs); err != nil {
		return nil, fsErr("rm", "%s", err)
	}
	r := newResult("rm", abs)
	r.Content = fmt.Sprintf("removed %s", abs)
	if count > 0 {
		r.Content = fmt.Sprintf("removed %s (%d items)", abs, count)
		r.set("items", count)
	}
	return r, nil
}

func countEntries(vfs ufs.FS, dir string) int {
	entries, err := vfs.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := len(entries)
	for _, e := range entries {
		if e.IsDir() {
			n += countEntries(vfs, dir+"/"+e.Name())
		}
	}
	return n
}

// ---- cp / mv（§4.8）----

// fsCp 实现 cp：{src, dst, recursive}。目标已存在报错（不覆盖）；目录需
// recursive=true；dst 为 src 自身或子路径报错；dst 父目录自动创建。
func fsCp(ctx context.Context, env *Env, p *fsParams) (*Result, error) {
	if p.Src == "" || p.Dst == "" {
		return nil, fsErr("cp", "src and dst are required")
	}
	src, dst, err := resolveSrcDst(env, "cp", p.Src, p.Dst)
	if err != nil {
		return nil, err
	}
	info, err := env.VFS.Stat(src)
	if err != nil {
		return nil, fsErr("cp", "cannot stat source %s: %s", src, err)
	}
	if info.IsDir() {
		if !p.Recursive {
			return nil, fsErr("cp", "%s is a directory (set recursive=true)", src)
		}
		if dst == src || strings.HasPrefix(dst, src+"/") {
			return nil, fsErr("cp", "cannot copy directory %s into itself: %s", src, dst)
		}
	}
	if _, err := env.VFS.Stat(dst); err == nil {
		return nil, fsErr("cp", "destination %s already exists", dst)
	}
	if info.IsDir() {
		if err := copyDir(env.VFS, src, dst); err != nil {
			return nil, err
		}
	} else {
		data, err := env.VFS.ReadFile(src)
		if err != nil {
			return nil, fsErr("cp", "cannot read source %s: %s", src, err)
		}
		if err := env.VFS.MkdirAll(dirOf(dst), 0o755); err != nil {
			return nil, fsErr("cp", "%s", err)
		}
		if err := env.VFS.WriteFile(dst, data, 0o644); err != nil {
			return nil, fsErr("cp", "cannot write destination %s: %s", dst, err)
		}
	}
	r := newResult("cp", dst)
	r.Attrs["source_path"] = src
	r.Content = fmt.Sprintf("copied %s to %s", src, dst)
	return r, nil
}

func copyDir(vfs ufs.FS, src, dst string) error {
	if err := vfs.MkdirAll(dst, 0o755); err != nil {
		return fsErr("cp", "cannot create directory %s: %s", dst, err)
	}
	entries, err := vfs.ReadDir(src)
	if err != nil {
		return fsErr("cp", "cannot read directory %s: %s", src, err)
	}
	for _, e := range entries {
		s, d := src+"/"+e.Name(), dst+"/"+e.Name()
		if e.IsDir() {
			if err := copyDir(vfs, s, d); err != nil {
				return err
			}
		} else {
			data, err := vfs.ReadFile(s)
			if err != nil {
				return fsErr("cp", "cannot read %s: %s", s, err)
			}
			if err := vfs.WriteFile(d, data, 0o644); err != nil {
				return fsErr("cp", "cannot write %s: %s", d, err)
			}
		}
	}
	return nil
}

// fsMv 实现 mv：{src, dst}。src==dst 报错；目标已存在报错；目录移入自身子路径
// 报错；dst 父目录自动创建；src 适用与 rm 同等的根目录硬保护。
func fsMv(ctx context.Context, env *Env, p *fsParams) (*Result, error) {
	if p.Src == "" || p.Dst == "" {
		return nil, fsErr("mv", "src and dst are required")
	}
	src, dst, err := resolveSrcDst(env, "mv", p.Src, p.Dst)
	if err != nil {
		return nil, err
	}
	if src == dst {
		return nil, fsErr("mv", "%s and %s are identical", src, dst)
	}
	if err := checkRootProtect(env, "mv", src); err != nil {
		return nil, err
	}
	info, err := env.VFS.Stat(src)
	if err != nil {
		return nil, fsErr("mv", "cannot stat source %s: %s", src, err)
	}
	if info.IsDir() && strings.HasPrefix(dst, src+"/") {
		return nil, fsErr("mv", "cannot move directory %s into itself: %s", src, dst)
	}
	if _, err := env.VFS.Stat(dst); err == nil {
		return nil, fsErr("mv", "destination %s already exists", dst)
	}
	if err := env.VFS.MkdirAll(dirOf(dst), 0o755); err != nil {
		return nil, fsErr("mv", "%s", err)
	}
	if err := env.VFS.Rename(src, dst); err != nil {
		return nil, fsErr("mv", "cannot move %s to %s: %s", src, dst, err)
	}
	r := newResult("mv", dst)
	r.Attrs["source_path"] = src
	r.Content = fmt.Sprintf("moved %s to %s", src, dst)
	return r, nil
}

func resolveSrcDst(env *Env, cmd, rawSrc, rawDst string) (string, string, error) {
	src, err := env.Resolve(rawSrc)
	if err != nil {
		return "", "", fsErr(cmd, "%s", err)
	}
	dst, err := env.Resolve(rawDst)
	if err != nil {
		return "", "", fsErr(cmd, "%s", err)
	}
	if err := env.CheckPath(cmd, src); err != nil {
		return "", "", err
	}
	if err := env.CheckPath(cmd, dst); err != nil {
		return "", "", err
	}
	return src, dst, nil
}
