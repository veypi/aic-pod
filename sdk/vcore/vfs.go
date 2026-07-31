package vcore

import (
	"io"
	"io/fs"
)

// VFS 是 vcore 全部指令作用的抽象文件系统（附录 C）。
// server 端适配 UFS（ufs.FS），pod 端适配 OS 本地路径（含路径沙箱），
// 测试使用 MemVFS。路径为斜杠分隔的绝对路径。
type VFS interface {
	Stat(name string) (fs.FileInfo, error)
	ReadDir(name string) ([]fs.DirEntry, error)
	ReadFile(name string) ([]byte, error)
	Open(name string) (fs.File, error)
	// Create 创建或截断文件（父目录必须存在）。
	Create(name string) (io.WriteCloser, error)
	// WriteFile 整文件写入（父目录必须存在）。
	WriteFile(name string, data []byte) error
	MkdirAll(name string) error
	RemoveAll(name string) error
	Rename(oldname, newname string) error
}

// Appender 是可选的真追加接口（O_APPEND）。
// 未实现时 fs write 的 append 模式退化为读改写（§4.3 实现取舍：
// cloud/page 读改写、物理 host 真追加；同 host 调用串行化已挡住并发交叉）。
type Appender interface {
	Append(name string, data []byte) error
}
