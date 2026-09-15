package host

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/vigo/contrib/ufs"
)

// OSVFS 是 OS 本地文件系统的 ufs.FS 适配（物理 host 执行环境）。
//
// 路径模型（2026-09-15 盘符方案）：
//   - 输入为 vcore 规范形：斜杠分隔绝对路径；Windows 盘符形 C:/…（裸 C: =
//     盘符根，严格语义——C:foo 盘符相对形态由 proto.ResolvePath 归为相对
//     路径，不会以盘符身份到达本层）；
//   - Windows 下 "/" 是虚拟挂载根：ReadDir 返回盘符挂载列表（C:、D:…），
//     Stat 返回合成目录信息；其余操作作用于 "/" 报错（rm/mv 另由
//     ProtectRoots 硬保护拦截）。"当前盘根" 语义不复存在；
//   - Windows 下非盘符绝对路径（/x）一律拒绝——虚拟根下只有盘符挂载。
type OSVFS struct{}

// errVirtualRoot 是 "/" 虚拟挂载根上执行非列举类操作的统一错误。
var errVirtualRoot = errors.New(`fs: "/" is a virtual drive-list root on this host (ls it to enumerate drives; file operations require a drive path like C:/…)`)

// isVirtualRoot 报告 name 是否为 windows 虚拟挂载根。
func isVirtualRoot(name string) bool { return runtime.GOOS == "windows" && name == "/" }

// toOS 把规范形路径映射为 OS 路径（非 Windows 为恒等映射；Windows 见 winToOS）。
func toOS(p string) (string, error) {
	if runtime.GOOS == "windows" {
		return winToOS(p)
	}
	return p, nil
}

// winDriveRe 匹配盘符规范形（斜杠已归一）：C:（裸盘符 = 盘符根）或 C:/…。
var winDriveRe = regexp.MustCompile(`^[A-Za-z]:($|/)`)

// winToOS 把规范形路径映射为 Windows OS 路径（纯函数，跨平台可测）：
//   - 归一与规则匹配层共用 proto.NormalizeDrivePath（本层兜底 ls/rg 递归拼接等
//     绕过主入口的产物；多斜杠前缀 //C:、盘符形内双分隔符 C://x 同样消除）；
//   - C: → C:\（盘根严格语义）；C:/x → C:\x；
//   - "/" → errVirtualRoot（Stat/ReadDir 特判，其余操作拒绝）；
//   - 其余 /…（非盘符绝对路径）→ 错误：虚拟根下只有盘符挂载。
func winToOS(p string) (string, error) {
	p = proto.NormalizeDrivePath(p)
	if p == "/" {
		return "", errVirtualRoot
	}
	if winDriveRe.MatchString(p) {
		drive := p[:1] + ":" // 盘符字母已在归一阶段大写
		// 分隔符转换不能用 filepath.FromSlash（非 windows 上是恒等映射，
		// 纯函数跨平台可测性会丢失）
		rest := strings.ReplaceAll(strings.TrimPrefix(p[2:], "/"), "/", `\`)
		if rest == "" {
			return drive + `\`, nil
		}
		return drive + `\` + rest, nil
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("fs: windows path requires a drive letter (C:/…); got %q", p)
	}
	return "", fmt.Errorf("fs: relative path %q reached the OS boundary", p)
}

// virtualDir 同时实现 fs.FileInfo 与 fs.DirEntry：虚拟挂载根（"/"）与盘符
// 挂载条目（"C:"、"D:"…）共用的合成目录节点（无磁盘实体，size 恒 0、
// mtime 恒 unix 0，ls 层容忍）。
type virtualDir string

func (v virtualDir) Name() string               { return string(v) }
func (v virtualDir) Size() int64                { return 0 }
func (v virtualDir) Mode() fs.FileMode          { return fs.ModeDir | 0o555 }
func (v virtualDir) ModTime() time.Time         { return time.Unix(0, 0) }
func (v virtualDir) IsDir() bool                { return true }
func (v virtualDir) Sys() any                   { return nil }
func (v virtualDir) Type() fs.FileMode          { return fs.ModeDir }
func (v virtualDir) Info() (fs.FileInfo, error) { return v, nil }

// driveEntries 把盘符列表格式化为虚拟根 ReadDir 条目（纯函数，跨平台可测）。
func driveEntries(drives []string) []fs.DirEntry {
	out := make([]fs.DirEntry, len(drives))
	for i, d := range drives {
		out[i] = virtualDir(d)
	}
	return out
}

// windowsDrives 探测存在的盘符：C–Z 逐个 stat("X:/")（裸 X: 是盘符当前目录
// 语义，不能用于探测）。返回盘符根规范形列表（"C:"、"D:"…，与 path.Clean 结果
// 一致）。filesystemRoots 与虚拟根 ReadDir 共用此探测（单一实现）。
func windowsDrives() []string {
	var drives []string
	for _, d := range "CDEFGHIJKLMNOPQRSTUVWXYZ" {
		if _, err := os.Stat(string(d) + ":/"); err == nil {
			drives = append(drives, string(d)+":")
		}
	}
	if len(drives) == 0 {
		drives = []string{"C:"}
	}
	return drives
}

func (OSVFS) Stat(name string) (fs.FileInfo, error) {
	if isVirtualRoot(name) {
		return virtualDir("/"), nil
	}
	p, err := toOS(name)
	if err != nil {
		return nil, err
	}
	return os.Stat(p)
}

func (OSVFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if isVirtualRoot(name) {
		return driveEntries(windowsDrives()), nil
	}
	p, err := toOS(name)
	if err != nil {
		return nil, err
	}
	return os.ReadDir(p)
}

func (OSVFS) ReadFile(name string) ([]byte, error) {
	p, err := toOS(name)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

func (OSVFS) Open(name string) (fs.File, error) {
	p, err := toOS(name)
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}

func (OSVFS) MkdirAll(name string, perm os.FileMode) error {
	p, err := toOS(name)
	if err != nil {
		return err
	}
	return os.MkdirAll(p, perm)
}

func (OSVFS) RemoveAll(name string) error {
	p, err := toOS(name)
	if err != nil {
		return err
	}
	return os.RemoveAll(p)
}

func (OSVFS) Rename(oldname, newname string) error {
	oldp, err := toOS(oldname)
	if err != nil {
		return err
	}
	newp, err := toOS(newname)
	if err != nil {
		return err
	}
	return os.Rename(oldp, newp)
}

// Create 创建或截断文件（*os.File 满足 ufs.File）。
func (OSVFS) Create(name string) (ufs.File, error) {
	p, err := toOS(name)
	if err != nil {
		return nil, err
	}
	return os.Create(p)
}

// WriteFile 整文件写入。
func (OSVFS) WriteFile(name string, data []byte, perm fs.FileMode) error {
	p, err := toOS(name)
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, perm)
}

// Search 委托 ufs 通用搜索实现。ufs.Search 内部 validatePath 会剥掉前导
// 斜杠（fs.FS 相对路径语义），经 osAbsFS 适配器补回绝对路径。
func (OSVFS) Search(searchPath, glob, pattern string, limit int, ignoreCase bool) ([]ufs.SearchMatch, error) {
	return ufs.Search(osAbsFS{}, searchPath, glob, pattern, limit, ignoreCase)
}

// osAbsFS 把 ufs.Search 产出的相对路径重新映射为 OS 绝对路径。
type osAbsFS struct{}

func (osAbsFS) Open(name string) (fs.File, error) {
	p, err := toOS("/" + name)
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}

func (osAbsFS) ReadDir(name string) ([]fs.DirEntry, error) {
	p, err := toOS("/" + name)
	if err != nil {
		return nil, err
	}
	return os.ReadDir(p)
}

func (osAbsFS) Stat(name string) (fs.FileInfo, error) {
	p, err := toOS("/" + name)
	if err != nil {
		return nil, err
	}
	return os.Stat(p)
}

// filesystemRoots 返回 rm/mv 根目录硬保护列表（§5.4：物理 host 文件系统根）。
// windows：盘符根（C:、D:…，规范形与 path.Clean 结果一致）+ 虚拟挂载根
// "/"——/ 变为虚拟根后 rm / 与 mv / 必须拒绝。
func filesystemRoots() []string {
	if runtime.GOOS == "windows" {
		return append(windowsDrives(), "/")
	}
	return []string{"/"}
}
