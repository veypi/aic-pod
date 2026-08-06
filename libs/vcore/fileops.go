package vcore

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/veypi/aic-pod/libs/proto"
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

// ---- rm（§5.4）----

// rm [-r] <path>：文件/空目录直接删；目录无 -r 报错；-r 递归删除。
func cmdRm(ctx context.Context, env *Env, argv []string) (*Result, error) {
	pa, err := parseArgv("rm", argvSpec{bools: map[string]bool{"-r": true}, minPos: 1, maxPos: 1}, argv)
	if err != nil {
		return nil, err
	}
	abs, err := env.Resolve(pa.pos[0])
	if err != nil {
		return nil, execErr("rm", "%s", err)
	}
	if err := env.CheckPath("rm", abs); err != nil {
		return nil, err
	}
	if err := checkRootProtect(env, "rm", abs); err != nil {
		return nil, err
	}
	info, err := env.VFS.Stat(abs)
	if err != nil {
		return nil, execErr("rm", "%s", err)
	}
	count := 0
	if info.IsDir() {
		entries, _ := env.VFS.ReadDir(abs)
		if len(entries) > 0 && !pa.bools["-r"] {
			return nil, execErr("rm", "%s is a non-empty directory (use -r)", abs)
		}
		if len(entries) > 0 {
			count = countEntries(env.VFS, abs)
		}
	}
	if err := env.VFS.RemoveAll(abs); err != nil {
		return nil, execErr("rm", "%s", err)
	}
	r := newResult("rm", abs)
	r.Content = fmt.Sprintf("removed %s", abs)
	if count > 0 {
		r.Content = fmt.Sprintf("removed %s (%d items)", abs, count)
		r.set("items", count)
	}
	return r, nil
}

func countEntries(vfs VFS, dir string) int {
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

// ---- mkdir（§5.4）----

// mkdir [-p] <path>：无 -p 时父目录必须存在、目标已存在报错；-p 递归创建且幂等。
func cmdMkdir(ctx context.Context, env *Env, argv []string) (*Result, error) {
	pa, err := parseArgv("mkdir", argvSpec{bools: map[string]bool{"-p": true}, minPos: 1, maxPos: 1}, argv)
	if err != nil {
		return nil, err
	}
	abs, err := env.Resolve(pa.pos[0])
	if err != nil {
		return nil, execErr("mkdir", "%s", err)
	}
	if err := env.CheckPath("mkdir", abs); err != nil {
		return nil, err
	}
	if _, err := env.VFS.Stat(abs); err == nil {
		if pa.bools["-p"] {
			r := newResult("mkdir", abs)
			r.Content = fmt.Sprintf("created %s", abs)
			return r, nil // -p 幂等成功
		}
		return nil, execErr("mkdir", "%s already exists", abs)
	}
	if !pa.bools["-p"] {
		if _, err := env.VFS.Stat(dirOf(abs)); err != nil {
			return nil, execErr("mkdir", "parent directory does not exist: %s", dirOf(abs))
		}
	}
	if err := env.VFS.MkdirAll(abs); err != nil {
		return nil, execErr("mkdir", "%s", err)
	}
	r := newResult("mkdir", abs)
	r.Content = fmt.Sprintf("created %s", abs)
	return r, nil
}

// ---- cp / mv（§5.4）----

// cp [-r] <src> <dst>：目标已存在报错（不覆盖）；目录需 -r；dst 为 src 自身或子路径报错。
func cmdCp(ctx context.Context, env *Env, argv []string) (*Result, error) {
	pa, err := parseArgv("cp", argvSpec{bools: map[string]bool{"-r": true}, minPos: 2, maxPos: 2}, argv)
	if err != nil {
		return nil, err
	}
	src, dst, err := resolveSrcDst(env, "cp", pa.pos[0], pa.pos[1])
	if err != nil {
		return nil, err
	}
	info, err := env.VFS.Stat(src)
	if err != nil {
		return nil, execErr("cp", "cannot stat source %s: %s", src, err)
	}
	if info.IsDir() {
		if !pa.bools["-r"] {
			return nil, execErr("cp", "%s is a directory (use -r)", src)
		}
		if dst == src || strings.HasPrefix(dst, src+"/") {
			return nil, execErr("cp", "cannot copy directory %s into itself: %s", src, dst)
		}
	}
	if _, err := env.VFS.Stat(dst); err == nil {
		return nil, execErr("cp", "destination %s already exists", dst)
	}
	if info.IsDir() {
		if err := copyDir(env.VFS, src, dst); err != nil {
			return nil, err
		}
	} else {
		data, err := env.VFS.ReadFile(src)
		if err != nil {
			return nil, execErr("cp", "cannot read source %s: %s", src, err)
		}
		if err := env.VFS.MkdirAll(dirOf(dst)); err != nil {
			return nil, execErr("cp", "%s", err)
		}
		if err := env.VFS.WriteFile(dst, data); err != nil {
			return nil, execErr("cp", "cannot write destination %s: %s", dst, err)
		}
	}
	r := newResult("cp", dst)
	r.Attrs["source_path"] = src
	r.Content = fmt.Sprintf("copied %s to %s", src, dst)
	return r, nil
}

func copyDir(vfs VFS, src, dst string) error {
	if err := vfs.MkdirAll(dst); err != nil {
		return execErr("cp", "cannot create directory %s: %s", dst, err)
	}
	entries, err := vfs.ReadDir(src)
	if err != nil {
		return execErr("cp", "cannot read directory %s: %s", src, err)
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
				return execErr("cp", "cannot read %s: %s", s, err)
			}
			if err := vfs.WriteFile(d, data); err != nil {
				return execErr("cp", "cannot write %s: %s", d, err)
			}
		}
	}
	return nil
}

// mv <src> <dst>：src==dst 报错；目标已存在报错；目录移入自身子路径报错；
// src 适用与 rm 同等的根目录硬保护（否则 rm 根保护可被 mv 绕过）。
func cmdMv(ctx context.Context, env *Env, argv []string) (*Result, error) {
	pa, err := parseArgv("mv", argvSpec{minPos: 2, maxPos: 2}, argv)
	if err != nil {
		return nil, err
	}
	src, dst, err := resolveSrcDst(env, "mv", pa.pos[0], pa.pos[1])
	if err != nil {
		return nil, err
	}
	if src == dst {
		return nil, execErr("mv", "%s and %s are identical", src, dst)
	}
	if err := checkRootProtect(env, "mv", src); err != nil {
		return nil, err
	}
	info, err := env.VFS.Stat(src)
	if err != nil {
		return nil, execErr("mv", "cannot stat source %s: %s", src, err)
	}
	if info.IsDir() && strings.HasPrefix(dst, src+"/") {
		return nil, execErr("mv", "cannot move directory %s into itself: %s", src, dst)
	}
	if _, err := env.VFS.Stat(dst); err == nil {
		return nil, execErr("mv", "destination %s already exists", dst)
	}
	if err := env.VFS.MkdirAll(dirOf(dst)); err != nil {
		return nil, execErr("mv", "%s", err)
	}
	if err := env.VFS.Rename(src, dst); err != nil {
		return nil, execErr("mv", "cannot move %s to %s: %s", src, dst, err)
	}
	r := newResult("mv", dst)
	r.Attrs["source_path"] = src
	r.Content = fmt.Sprintf("moved %s to %s", src, dst)
	return r, nil
}

func resolveSrcDst(env *Env, cmd, rawSrc, rawDst string) (string, string, error) {
	src, err := env.Resolve(rawSrc)
	if err != nil {
		return "", "", execErr(cmd, "%s", err)
	}
	dst, err := env.Resolve(rawDst)
	if err != nil {
		return "", "", execErr(cmd, "%s", err)
	}
	if err := env.CheckPath(cmd, src); err != nil {
		return "", "", err
	}
	if err := env.CheckPath(cmd, dst); err != nil {
		return "", "", err
	}
	return src, dst, nil
}
