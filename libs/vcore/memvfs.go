package vcore

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/veypi/vigo/contrib/ufs"
)

// MemVFS 是内存 ufs.FS 实现：测试与一致性向量运行器使用（JS 端对齐同一行为）。
type MemVFS struct {
	root *memNode
}

type memNode struct {
	dir      bool
	data     []byte
	modTime  time.Time
	children map[string]*memNode
}

// NewMemVFS 创建空内存文件系统（根目录已存在）。
func NewMemVFS() *MemVFS {
	return &MemVFS{root: &memNode{dir: true, children: map[string]*memNode{}}}
}

// SetFile 写入文件并设定 mtime（向量装载用，父目录自动创建）。
func (m *MemVFS) SetFile(name string, data []byte, modTime time.Time) {
	_ = m.MkdirAll(path.Dir(name), 0o755)
	n, _ := m.lookup(path.Dir(name))
	base := path.Base(name)
	n.children[base] = &memNode{data: data, modTime: modTime}
}

// SetDir 创建目录并设定 mtime（向量装载用）。
func (m *MemVFS) SetDir(name string, modTime time.Time) {
	_ = m.MkdirAll(name, 0o755)
	if n, _ := m.lookup(name); n != nil {
		n.modTime = modTime
	}
}

func (m *MemVFS) lookup(name string) (*memNode, error) {
	name = path.Clean(name)
	if name == "/" {
		return m.root, nil
	}
	cur := m.root
	for _, seg := range strings.Split(strings.TrimPrefix(name, "/"), "/") {
		if !cur.dir {
			return nil, fmt.Errorf("not a directory: %s", name)
		}
		next, ok := cur.children[seg]
		if !ok {
			return nil, fs.ErrNotExist
		}
		cur = next
	}
	return cur, nil
}

func (m *MemVFS) Stat(name string) (fs.FileInfo, error) {
	n, err := m.lookup(name)
	if err != nil {
		return nil, err
	}
	return memInfo{name: path.Base(name), node: n}, nil
}

func (m *MemVFS) ReadDir(name string) ([]fs.DirEntry, error) {
	n, err := m.lookup(name)
	if err != nil {
		return nil, err
	}
	if !n.dir {
		return nil, fmt.Errorf("not a directory: %s", name)
	}
	names := make([]string, 0, len(n.children))
	for k := range n.children {
		names = append(names, k)
	}
	sort.Strings(names) // fs.ReadDir 语义：按文件名排序
	out := make([]fs.DirEntry, 0, len(names))
	for _, k := range names {
		out = append(out, memInfo{name: k, node: n.children[k]})
	}
	return out, nil
}

func (m *MemVFS) ReadFile(name string) ([]byte, error) {
	n, err := m.lookup(name)
	if err != nil {
		return nil, err
	}
	if n.dir {
		return nil, fmt.Errorf("is a directory: %s", name)
	}
	return append([]byte(nil), n.data...), nil
}

func (m *MemVFS) Open(name string) (fs.File, error) {
	n, err := m.lookup(name)
	if err != nil {
		return nil, err
	}
	if n.dir {
		return nil, fmt.Errorf("is a directory: %s", name)
	}
	return &memFile{Reader: strings.NewReader(string(n.data)), info: memInfo{name: path.Base(name), node: n}}, nil
}

func (m *MemVFS) Create(name string) (ufs.File, error) {
	parent, err := m.lookup(path.Dir(name))
	if err != nil {
		return nil, err
	}
	if !parent.dir {
		return nil, fmt.Errorf("not a directory: %s", path.Dir(name))
	}
	n := &memNode{modTime: time.Now()}
	parent.children[path.Base(name)] = n
	return &memRWFile{node: n, name: path.Base(name)}, nil
}

func (m *MemVFS) WriteFile(name string, data []byte, _ fs.FileMode) error {
	w, err := m.Create(name)
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	return w.Close()
}

// Search 委托 ufs 通用搜索实现。
func (m *MemVFS) Search(searchPath, glob, pattern string, limit int, ignoreCase bool) ([]ufs.SearchMatch, error) {
	return ufs.Search(m, searchPath, glob, pattern, limit, ignoreCase)
}

func (m *MemVFS) Append(name string, data []byte) error {
	n, err := m.lookup(name)
	if err == fs.ErrNotExist {
		return m.WriteFile(name, data, 0o644)
	}
	if err != nil {
		return err
	}
	if n.dir {
		return fmt.Errorf("is a directory: %s", name)
	}
	n.data = append(n.data, data...)
	n.modTime = time.Now()
	return nil
}

func (m *MemVFS) MkdirAll(name string, _ os.FileMode) error {
	name = path.Clean(name)
	if name == "/" {
		return nil
	}
	cur := m.root
	for _, seg := range strings.Split(strings.TrimPrefix(name, "/"), "/") {
		next, ok := cur.children[seg]
		if !ok {
			next = &memNode{dir: true, children: map[string]*memNode{}, modTime: time.Now()}
			cur.children[seg] = next
		}
		if !next.dir {
			return fmt.Errorf("not a directory: %s", name)
		}
		cur = next
	}
	return nil
}

func (m *MemVFS) RemoveAll(name string) error {
	parent, err := m.lookup(path.Dir(name))
	if err != nil {
		return err
	}
	delete(parent.children, path.Base(name))
	return nil
}

func (m *MemVFS) Rename(oldname, newname string) error {
	src, err := m.lookup(oldname)
	if err != nil {
		return err
	}
	dstParent, err := m.lookup(path.Dir(newname))
	if err != nil {
		return err
	}
	if !dstParent.dir {
		return fmt.Errorf("not a directory: %s", path.Dir(newname))
	}
	srcParent, _ := m.lookup(path.Dir(oldname))
	delete(srcParent.children, path.Base(oldname))
	dstParent.children[path.Base(newname)] = src
	return nil
}

// ---- fs.FileInfo / fs.DirEntry / fs.File 适配 ----

type memInfo struct {
	name string
	node *memNode
}

func (i memInfo) Name() string      { return i.name }
func (i memInfo) Size() int64       { return int64(len(i.node.data)) }
func (i memInfo) Mode() fs.FileMode { return 0o644 }
func (i memInfo) ModTime() time.Time {
	return i.node.modTime
}
func (i memInfo) IsDir() bool { return i.node.dir }
func (i memInfo) Sys() any    { return nil }
func (i memInfo) Type() fs.FileMode {
	if i.node.dir {
		return fs.ModeDir
	}
	return 0
}
func (i memInfo) Info() (fs.FileInfo, error) { return i, nil }

type memFile struct {
	*strings.Reader
	info memInfo
}

func (f *memFile) Close() error               { return nil }
func (f *memFile) Stat() (fs.FileInfo, error) { return f.info, nil }

// memRWFile 实现 ufs.File（Read/Write/Seek/Stat/Close）：直写节点数据。
type memRWFile struct {
	node *memNode
	name string
	off  int64
}

func (f *memRWFile) Read(p []byte) (int, error) {
	if f.off >= int64(len(f.node.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.node.data[f.off:])
	f.off += int64(n)
	return n, nil
}

func (f *memRWFile) Write(p []byte) (int, error) {
	end := f.off + int64(len(p))
	if end > int64(len(f.node.data)) {
		grown := make([]byte, end)
		copy(grown, f.node.data)
		f.node.data = grown
	}
	copy(f.node.data[f.off:], p)
	f.off = end
	f.node.modTime = time.Now()
	return len(p), nil
}

func (f *memRWFile) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = f.off + offset
	case io.SeekEnd:
		abs = int64(len(f.node.data)) + offset
	}
	if abs < 0 {
		return 0, fmt.Errorf("negative position")
	}
	f.off = abs
	return abs, nil
}

func (f *memRWFile) Stat() (fs.FileInfo, error) {
	return memInfo{name: f.name, node: f.node}, nil
}

func (f *memRWFile) Close() error { return nil }
