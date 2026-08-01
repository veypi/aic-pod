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
