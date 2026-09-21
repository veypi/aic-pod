package hostfs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	tool "github.com/veypi/aic-pod/libs/hosts_tool"
	"github.com/veypi/aic-pod/libs/proto"
	fsp "github.com/veypi/aic-pod/protocol/fs"
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
func (f *FS) View(ctx context.Context, c tool.Caller) ufs.FS {
	return &fileView{f: f, ctx: ctx, call: Call{Caller: c, Owner: Owner(c), Command: "fs"}, versions: map[string]string{}}
}

type fileView struct {
	f        *FS
	ctx      context.Context
	call     Call
	mu       sync.Mutex
	versions map[string]string
}

func (v *fileView) location(name string) (fsp.Path, error) {
	if runtime.GOOS == "windows" {
		name = proto.NormalizeDrivePath(name)
		if len(name) < 2 || name[1] != ':' {
			return fsp.Path{}, fsp.Fail("invalid_argument", "Windows file operations require a drive path")
		}
		if len(name) == 2 {
			name += "/"
		}
	}
	abs, err := filepath.Abs(filepath.FromSlash(name))
	if err != nil {
		return fsp.Path{}, err
	}
	for _, r := range v.f.roots {
		rel, err := filepath.Rel(r.Path, abs)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			segments := []string{}
			if rel != "." {
				segments = strings.Split(rel, string(filepath.Separator))
			}
			p := fsp.Path{RootID: r.ID, Segments: segments}
			return p, p.Validate(runtime.GOOS == "windows")
		}
	}
	return fsp.Path{}, fsp.Fail("permission_denied", "Path is outside filesystem roots")
}
func (v *fileView) invoke(method string, args any) (any, error) {
	raw, _ := json.Marshal(args)
	call := v.call
	call.Method = method
	call.Args = raw
	return v.f.Run(v.ctx, call)
}
func (v *fileView) remember(name string, info fs.FileInfo) {
	v.mu.Lock()
	v.versions[name] = version(info)
	v.mu.Unlock()
}
func (v *fileView) Stat(name string) (fs.FileInfo, error) {
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
func (v *fileView) Open(name string) (fs.File, error) {
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
		return nil, fsp.Fail("unsupported", "Text reads require a regular file")
	}
	file, err := openRegular(h, base)
	if err != nil {
		return nil, err
	}
	actual, err := file.Stat()
	if err != nil || !os.SameFile(actual, info) || version(actual) != version(info) {
		file.Close()
		return nil, fsp.Fail("source_changed", "File changed during open")
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
			return fsp.Fail("source_changed", "File changed while reading")
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
		return nil, fsp.Fail("output_limit", "Text file exceeds 64 MiB")
	}
	return data, err
}
func (v *fileView) ReadDir(name string) ([]fs.DirEntry, error) {
	if runtime.GOOS == "windows" && name == "/" {
		if err := v.ctx.Err(); err != nil {
			return nil, err
		}
		v.f.mu.Lock()
		defer v.f.mu.Unlock()
		out := []fs.DirEntry{}
		for _, root := range v.f.roots {
			if _, _, err := v.f.check(v.ctx, v.call, fsp.Path{RootID: root.ID}, false); err == nil {
				out = append(out, virtualDir(filepath.VolumeName(root.Path)))
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
		return nil, fsp.Fail("overloaded", "Directory limit")
	}
	out := []fs.DirEntry{}
	for _, entry := range entries {
		child := fsp.Path{RootID: p.RootID, Segments: append(append([]string{}, p.Segments...), entry.Name())}
		if _, _, err := v.f.check(v.ctx, v.call, child, false); err == nil {
			out = append(out, entry)
		}
	}
	return out, nil
}
func (v *fileView) condition(name string) (fsp.Condition, error) {
	v.mu.Lock()
	observed := v.versions[name]
	v.mu.Unlock()
	if observed != "" {
		return fsp.Condition{Version: observed}, nil
	}
	info, err := v.Stat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return fsp.Condition{Absent: true}, nil
	}
	if err != nil {
		return fsp.Condition{}, err
	}
	return fsp.Condition{Version: version(info)}, nil
}
func (v *fileView) commit(name string, r io.Reader, size int64, condition fsp.Condition) error {
	p, err := v.location(name)
	if err != nil {
		return err
	}
	source, err := v.f.cfg.Bytes.Upload(v.ctx, v.call.Owner, r, &size, "", "")
	if err != nil {
		return err
	}
	defer v.f.cfg.Bytes.Release(v.call.Owner, source.Ref)
	_, err = v.invoke("write", writeArgs{Path: p, Source: source.Ref, Condition: condition})
	return err
}
func (v *fileView) WriteFile(name string, data []byte, perm fs.FileMode) error {
	condition, err := v.condition(name)
	if err != nil {
		return err
	}
	return v.commit(name, bytes.NewReader(data), int64(len(data)), condition)
}
func (v *fileView) Create(name string) (ufs.File, error) {
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
		return 0, fsp.Fail("output_limit", "Text output exceeds 64 MiB")
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
func (v *fileView) MkdirAll(name string, perm fs.FileMode) error {
	p, err := v.location(name)
	if err != nil {
		return err
	}
	if len(p.Segments) == 0 {
		return nil
	}
	_, err = v.invoke("mkdir", mkdirArgs{Path: p, Parents: true, ExistOK: true})
	return err
}
func (v *fileView) RemoveAll(name string) error {
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
	_, err = v.invoke("remove", removeArgs{Path: p, IfVersion: version(info), Recursive: true, MissingOK: true})
	return err
}
func (v *fileView) Rename(from, to string) error {
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
	_, err = v.invoke("move", moveArgs{Src: src, Dst: dst, IfVersion: version(info), Condition: condition})
	return err
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
