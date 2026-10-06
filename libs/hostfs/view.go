package hostfs

import (
	"bytes"
	"context"
	"errors"
	"github.com/veypi/aic-pod/protocol"

	"github.com/veypi/vigo/contrib/ufs"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// View adapts text editing/search to the same FS implementation used by RTC.
// Versions observed during this invocation become preconditions on its writes.
func (f *FS) View(ctx context.Context, c protocol.Caller) ufs.FS {
	return &fileView{f: f, ctx: ctx, call: Call{Caller: c, Owner: Owner(c), Command: "fs"}, versions: map[string]string{}}
}

type fileView struct {
	f        *FS
	ctx      context.Context
	call     Call
	mu       sync.Mutex
	versions map[string]string
}

func (v *fileView) location(name string) (protocol.FSPath, error) {
	if runtime.GOOS == "windows" {
		name = protocol.NormalizeHostPath(name)
		if q, ok := protocol.WinTmpToOS(name, os.TempDir()); ok {
			// /tmp 虚拟别名（cygwin 式映射 os.TempDir()，与 Decide 侧
			// vbox.HostPathToOS 同口径）。
			name = q
		} else {
			drive, rest, ok := protocol.SplitDriveRoot(name)
			if !ok {
				return protocol.FSPath{}, protocol.FSFail("invalid_argument", "Windows file operations require a drive path (/c/…)")
			}
			name = strings.ToUpper(string(drive)) + `:\` + strings.ReplaceAll(rest, "/", `\`)
		}
	}
	abs, err := filepath.Abs(filepath.FromSlash(name))
	if err != nil {
		return protocol.FSPath{}, err
	}
	for _, r := range v.f.roots {
		rel, err := filepath.Rel(r.Path, abs)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			segments := []string{}
			if rel != "." {
				segments = strings.Split(rel, string(filepath.Separator))
			}
			p := protocol.FSPath{RootID: r.ID, Segments: segments}
			return p, p.Validate(runtime.GOOS == "windows")
		}
	}
	return protocol.FSPath{}, protocol.FSFail("permission_denied", "Path is outside filesystem roots")
}

// call 返回以 method 标记的调用上下文（类型化入口与 JSON 分发共用同一
// 具体方法与校验授权链——批次 2 解除「编码 JSON → 重新解析」的内部回环）。
func (v *fileView) callAs(method string) Call {
	call := v.call
	call.Method = method
	return call
}
func (v *fileView) remember(name string, info fs.FileInfo) {
	v.mu.Lock()
	v.versions[name] = version(info)
	v.mu.Unlock()
}

// echoPath 把底层错误里的路径改写为调用者传入的形态（fileView 各方法用
// `defer func() { err = echoPath(name, err) }()` 统一回显）：pod 侧 os.Root 系调用
// 只报根内相对名（"statat fsedit-nope: no such file or directory"），调用者（平台 fs
// 工具）拿到这种路径无法纠偏。非 *fs.PathError（规则表拒绝、协议 Fault、context
// 取消等）原样透出。
func echoPath(name string, err error) error {
	if err == nil {
		return nil
	}
	var pe *fs.PathError
	if !errors.As(err, &pe) {
		return err
	}
	return &fs.PathError{Op: pe.Op, Path: name, Err: pe.Err}
}

func (v *fileView) Stat(name string) (result fs.FileInfo, err error) {
	defer func() { err = echoPath(name, err) }()
	if runtime.GOOS == "windows" && name == "/" {
		return virtualDir("/"), v.ctx.Err()
	}
	p, err := v.location(name)
	if err != nil {
		return nil, err
	}
	v.f.mu.Lock()
	defer v.f.mu.Unlock()
	info, err := v.f.info(v.ctx, v.call, p, false)
	if err == nil {
		v.remember(name, info)
	}
	return info, err
}
func (v *fileView) Open(name string) (result fs.File, err error) {
	defer func() { err = echoPath(name, err) }()
	p, err := v.location(name)
	if err != nil {
		return nil, err
	}
	v.f.mu.Lock()
	defer v.f.mu.Unlock()
	r, abs, err := v.f.check(v.ctx, v.call, p, false)
	if err != nil {
		return nil, err
	}
	h, base, err := parent(r, abs)
	if err != nil {
		return nil, err
	}
	defer h.Close()
	info, err := h.Lstat(base)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, protocol.FSFail("unsupported", "Text reads require a regular file")
	}
	file, err := openRegular(h, base)
	if err != nil {
		return nil, err
	}
	actual, err := file.Stat()
	if err != nil || !os.SameFile(actual, info) || version(actual) != version(info) {
		file.Close()
		return nil, protocol.FSFail("source_changed", "File changed during open")
	}
	v.remember(name, info)
	return &checkedFile{File: file, check: func() error {
		if err := v.ctx.Err(); err != nil {
			return err
		}
		if err := v.f.cfg.Check(v.ctx, v.call, abs, false); err != nil {
			return err
		}
		current, err := file.Stat()
		if err != nil {
			return err
		}
		if version(current) != version(info) {
			return protocol.FSFail("source_changed", "File changed while reading")
		}
		return nil
	}}, nil
}

type checkedFile struct {
	*os.File
	check func() error
}

func (f *checkedFile) Read(p []byte) (int, error) {
	if err := f.check(); err != nil {
		return 0, err
	}
	n, err := f.File.Read(p)
	if verify := f.check(); verify != nil {
		return 0, verify
	}
	return n, err
}
func (v *fileView) ReadFile(name string) ([]byte, error) {
	f, err := v.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 64<<20+1))
	if len(data) > 64<<20 {
		return nil, protocol.FSFail("output_limit", "Text file exceeds 64 MiB")
	}
	return data, err
}
func (v *fileView) ReadDir(name string) (result []fs.DirEntry, err error) {
	defer func() { err = echoPath(name, err) }()
	if runtime.GOOS == "windows" && name == "/" {
		if err := v.ctx.Err(); err != nil {
			return nil, err
		}
		v.f.mu.Lock()
		defer v.f.mu.Unlock()
		out := []fs.DirEntry{}
		for _, root := range v.f.roots {
			if _, _, err := v.f.check(v.ctx, v.call, protocol.FSPath{RootID: root.ID}, false); err == nil {
				// 条目名为规范形首段（"c"——子路径拼接得 /c 规范形）。
				out = append(out, virtualDir(strings.ToLower(strings.TrimSuffix(filepath.VolumeName(root.Path), ":"))))
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
		return out, nil
	}
	p, err := v.location(name)
	if err != nil {
		return nil, err
	}
	v.f.mu.Lock()
	defer v.f.mu.Unlock()
	dir, _, err := v.f.directory(v.ctx, v.call, p, false)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	file, err := dir.Open(".")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	entries, err := file.ReadDir(v.f.cfg.MaxDirectoryEntries + 1)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(entries) > v.f.cfg.MaxDirectoryEntries {
		return nil, protocol.FSFail("overloaded", "Directory limit")
	}
	out := []fs.DirEntry{}
	for _, entry := range entries {
		child := protocol.FSPath{RootID: p.RootID, Segments: append(append([]string{}, p.Segments...), entry.Name())}
		if _, _, err := v.f.check(v.ctx, v.call, child, false); err == nil {
			out = append(out, entry)
		}
	}
	return out, nil
}
func (v *fileView) condition(name string) (protocol.FSCondition, error) {
	v.mu.Lock()
	observed := v.versions[name]
	v.mu.Unlock()
	if observed != "" {
		return protocol.FSCondition{Version: observed}, nil
	}
	info, err := v.Stat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return protocol.FSCondition{Absent: true}, nil
	}
	if err != nil {
		return protocol.FSCondition{}, err
	}
	return protocol.FSCondition{Version: version(info)}, nil
}
func (v *fileView) commit(name string, r io.Reader, size int64, condition protocol.FSCondition) error {
	p, err := v.location(name)
	if err != nil {
		return err
	}
	source, err := v.f.cfg.Bytes.Upload(v.ctx, v.call.Owner, r, &size, "", "")
	if err != nil {
		return err
	}
	defer v.f.cfg.Bytes.Release(v.call.Owner, source.Ref)
	return v.f.writeTyped(v.ctx, v.callAs("write"), writeArgs{Path: p, Source: source.Ref, Condition: condition})
}
func (v *fileView) WriteFile(name string, data []byte, perm fs.FileMode) (err error) {
	defer func() { err = echoPath(name, err) }()
	condition, err := v.condition(name)
	if err != nil {
		return err
	}
	return v.commit(name, bytes.NewReader(data), int64(len(data)), condition)
}
func (v *fileView) Create(name string) (result ufs.File, err error) {
	defer func() { err = echoPath(name, err) }()
	condition, err := v.condition(name)
	if err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(v.f.cfg.Bytes.dir, "text-")
	if err != nil {
		return nil, err
	}
	return &commitFile{File: file, commit: func() error {
		info, err := file.Stat()
		if err != nil {
			return err
		}
		return v.commit(name, io.NewSectionReader(file, 0, info.Size()), info.Size(), condition)
	}}, nil
}

type commitFile struct {
	*os.File
	commit func() error
	closed bool
}

func (f *commitFile) Write(p []byte) (int, error) {
	pos, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	if pos+int64(len(p)) > 64<<20 {
		return 0, protocol.FSFail("output_limit", "Text output exceeds 64 MiB")
	}
	return f.File.Write(p)
}
func (f *commitFile) Close() error {
	if f.closed {
		return nil
	}
	f.closed = true
	err := f.commit()
	f.File.Close()
	os.Remove(f.Name())
	return err
}
func (v *fileView) MkdirAll(name string, perm fs.FileMode) (err error) {
	defer func() { err = echoPath(name, err) }()
	p, err := v.location(name)
	if err != nil {
		return err
	}
	if len(p.Segments) == 0 {
		return nil
	}
	return v.f.mkdirTyped(v.ctx, v.callAs("mkdir"), mkdirArgs{Path: p, Parents: true, ExistOK: true})
}
func (v *fileView) RemoveAll(name string) (err error) {
	defer func() { err = echoPath(name, err) }()
	p, err := v.location(name)
	if err != nil {
		return err
	}
	info, err := v.Stat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return v.f.removeTyped(v.ctx, v.callAs("remove"), removeArgs{Path: p, IfVersion: version(info), Recursive: true, MissingOK: true})
}
func (v *fileView) Rename(from, to string) (err error) {
	// 报错回显源路径（move 类失败以 src 侧为主）。
	defer func() { err = echoPath(from, err) }()
	src, err := v.location(from)
	if err != nil {
		return err
	}
	dst, err := v.location(to)
	if err != nil {
		return err
	}
	info, err := v.Stat(from)
	if err != nil {
		return err
	}
	condition, err := v.condition(to)
	if err != nil {
		return err
	}
	return v.f.moveTyped(v.ctx, v.callAs("move"), moveArgs{Src: src, Dst: dst, IfVersion: version(info), Condition: condition})
}
func (v *fileView) Search(path, glob, pattern string, limit int, ignore bool) ([]ufs.SearchMatch, error) {
	return ufs.Search(searchView{v}, path, glob, pattern, limit, ignore)
}

type searchView struct{ *fileView }

func (v searchView) Open(name string) (fs.File, error) { return v.fileView.Open("/" + name) }
func (v searchView) ReadDir(name string) ([]fs.DirEntry, error) {
	return v.fileView.ReadDir("/" + name)
}
func (v searchView) Stat(name string) (fs.FileInfo, error) { return v.fileView.Stat("/" + name) }

// Synthetic drive listing is FS domain data, with no mutable resource handle.
type virtualDir string

func (v virtualDir) Name() string               { return string(v) }
func (v virtualDir) Size() int64                { return 0 }
func (v virtualDir) Mode() fs.FileMode          { return fs.ModeDir | 0555 }
func (v virtualDir) ModTime() time.Time         { return time.Unix(0, 0) }
func (v virtualDir) IsDir() bool                { return true }
func (v virtualDir) Sys() any                   { return nil }
func (v virtualDir) Type() fs.FileMode          { return fs.ModeDir }
func (v virtualDir) Info() (fs.FileInfo, error) { return v, nil }
