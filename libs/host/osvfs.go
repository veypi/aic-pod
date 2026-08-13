package host

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"github.com/veypi/vigo/contrib/ufs"
)

// OSVFS 是 OS 本地文件系统的 ufs.FS 适配（物理 host 执行环境）。
// 路径为斜杠分隔的绝对路径（Windows 下经 filepath 转换）。
type OSVFS struct{}

func toOS(p string) string {
	if runtime.GOOS == "windows" {
		return filepath.FromSlash(p)
	}
	return p
}

func (OSVFS) Stat(name string) (fs.FileInfo, error)      { return os.Stat(toOS(name)) }
func (OSVFS) ReadDir(name string) ([]fs.DirEntry, error) { return os.ReadDir(toOS(name)) }
func (OSVFS) ReadFile(name string) ([]byte, error)       { return os.ReadFile(toOS(name)) }
func (OSVFS) Open(name string) (fs.File, error)          { return os.Open(toOS(name)) }

func (OSVFS) MkdirAll(name string, perm os.FileMode) error {
	return os.MkdirAll(toOS(name), perm)
}
func (OSVFS) RemoveAll(name string) error { return os.RemoveAll(toOS(name)) }
func (OSVFS) Rename(oldname, newname string) error {
	return os.Rename(toOS(oldname), toOS(newname))
}

// Create 创建或截断文件（*os.File 满足 ufs.File）。
func (OSVFS) Create(name string) (ufs.File, error) { return os.Create(toOS(name)) }

// WriteFile 整文件写入。
func (OSVFS) WriteFile(name string, data []byte, perm fs.FileMode) error {
	return os.WriteFile(toOS(name), data, perm)
}

// Search 委托 ufs 通用搜索实现。ufs.Search 内部 validatePath 会剥掉前导
// 斜杠（fs.FS 相对路径语义），经 osAbsFS 适配器补回绝对路径。
func (OSVFS) Search(searchPath, glob, pattern string, limit int, ignoreCase bool) ([]ufs.SearchMatch, error) {
	return ufs.Search(osAbsFS{}, searchPath, glob, pattern, limit, ignoreCase)
}

// osAbsFS 把 ufs.Search 产出的相对路径重新映射为 OS 绝对路径。
type osAbsFS struct{}

func (osAbsFS) Open(name string) (fs.File, error) { return os.Open(toOS("/" + name)) }
func (osAbsFS) ReadDir(name string) ([]fs.DirEntry, error) {
	return os.ReadDir(toOS("/" + name))
}
func (osAbsFS) Stat(name string) (fs.FileInfo, error) { return os.Stat(toOS("/" + name)) }

// filesystemRoots 返回 rm/mv 根目录硬保护列表（§5.4：物理 host 文件系统根）。
// 盘符根规范形为 "C:"：path.Clean 会剥掉盘符后的尾斜杠（"C:/" → "C:"），
// proto.ResolvePath 对盘符路径归一反斜杠后输出即此形，等值比较可命中。
// Stat 用 "C:/" 探测盘符存在性（裸 "C:" 指盘符当前目录，语义不同）。
func filesystemRoots() []string {
	if runtime.GOOS == "windows" {
		var roots []string
		for _, d := range "CDEFGHIJKLMNOPQRSTUVWXYZ" {
			if _, err := os.Stat(string(d) + ":/"); err == nil {
				roots = append(roots, string(d)+":")
			}
		}
		if len(roots) == 0 {
			roots = []string{"C:"}
		}
		return roots
	}
	return []string{"/"}
}
