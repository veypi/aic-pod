package host

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// OSVFS 是 OS 本地文件系统的 vcore.VFS 适配（物理 host 执行环境）。
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
func (OSVFS) MkdirAll(name string) error                 { return os.MkdirAll(toOS(name), 0o755) }
func (OSVFS) RemoveAll(name string) error                { return os.RemoveAll(toOS(name)) }
func (OSVFS) Rename(oldname, newname string) error       { return os.Rename(toOS(oldname), toOS(newname)) }

// Create 创建或截断文件。
func (OSVFS) Create(name string) (io.WriteCloser, error) { return os.Create(toOS(name)) }

// WriteFile 整文件写入。
func (OSVFS) WriteFile(name string, data []byte) error {
	return os.WriteFile(toOS(name), data, 0o644)
}

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
