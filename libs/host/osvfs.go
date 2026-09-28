package host

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/vigo/contrib/ufs"
)

// OSVFS 是 OS 本地文件系统的 ufs.FS 适配（物理 host 执行环境）。
//
// 路径模型（2026-09-24 全局统一 /c/ 类 Linux 规范形，废除 C:/ 盘符形）：
//   - 输入为公共规范形：斜杠分隔绝对路径；Windows 盘符 = 首段单字母
//     （/c/…，/c = 盘符根）。C:/…、C:\… 等输入形由 proto.NormalizeHostPath
//     容错归一；
//   - Windows 下 "/" 是虚拟挂载根：ReadDir 返回盘符挂载列表（c、d…），
//     Stat 返回合成目录信息；其余操作作用于 "/" 报错；
//   - Windows 下 /tmp 是虚拟别名（cygwin 式语义）：映射到 os.TempDir()，
//     与规则表侧 canonical（proto.HostPathToOS）同口径——vsh 引擎布局
//     初始化与脚本的 /tmp 写在 win 上有真实落点；
//   - Windows 下非盘符绝对路径（/x）一律拒绝——虚拟根下只有盘符挂载。
type OSVFS struct{}

// errVirtualRoot 是 "/" 虚拟挂载根上执行非列举类操作的统一错误。
var errVirtualRoot = errors.New(`fs: "/" is a virtual drive-list root on this host (ls it to enumerate drives; file operations require a drive path like /c/…)`)

// isVirtualRoot 报告 name 是否为 windows 虚拟挂载根。
func isVirtualRoot(name string) bool { return runtime.GOOS == "windows" && name == "/" }

// toOS 把规范形路径映射为 OS 路径（非 Windows 为恒等映射；Windows 见 winToOS）。
func toOS(p string) (string, error) {
	if runtime.GOOS == "windows" {
		p = proto.NormalizeHostPath(p)
		// /tmp 虚拟别名优先于盘根判定（winToOS 对非盘符绝对路径报错）。
		if q, ok := proto.WinTmpToOS(p, os.TempDir()); ok {
			return q, nil
		}
		return winToOS(p)
	}
	return p, nil
}

// winToOS 把规范形路径映射为 Windows OS 路径（纯函数，跨平台可测）：
//   - 归一与规则匹配层共用 proto.NormalizeHostPath（本层兜底 ls/rg 递归拼接等
//     绕过主入口的产物；多斜杠前缀 //c、旧输入形 C:/… 同样归一）；
//   - /c → C:\（盘根）；/c/x → C:\x；
//   - "/" → errVirtualRoot（Stat/ReadDir 特判，其余操作拒绝）；
//   - 其余 /…（非盘符绝对路径）→ 错误：虚拟根下只有盘符挂载。
func winToOS(p string) (string, error) {
	p = proto.NormalizeHostPath(p)
	if p == "/" {
		return "", errVirtualRoot
	}
	drive, rest, ok := proto.SplitDriveRoot(p)
	if ok {
		d := strings.ToUpper(string(drive)) + ":"
		// 分隔符转换不能用 filepath.FromSlash（非 windows 上是恒等映射，
		// 纯函数跨平台可测性会丢失）
		rest = strings.ReplaceAll(rest, "/", `\`)
		if rest == "" {
			return d + `\`, nil
		}
		return d + `\` + rest, nil
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("fs: windows path requires a drive root (/c/…); got %q", p)
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

// driveEntries 把盘符列表格式化为虚拟根 ReadDir 条目（纯函数，跨平台可测）：
// 条目名为规范形首段（"c"、"d"…——子路径拼接得 /c、/d 规范形）。
func driveEntries(drives []string) []fs.DirEntry {
	out := make([]fs.DirEntry, len(drives))
	for i, d := range drives {
		out[i] = virtualDir(strings.ToLower(strings.TrimSuffix(d, ":")))
	}
	return out
}

// windowsDrives 探测存在的盘符：C–Z 逐个 stat("X:/")（裸 X: 是盘符当前目录
// 语义，不能用于探测）。返回盘符根原生态列表（"C:"、"D:"…——mount
// （deviceFileRoots）与探测共用；虚拟根 ReadDir 经 driveEntries 转规范形名）。
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

// Chmod 真实修改权限位（host OS 可持久化执行位——vsh 脚本 chmod +x 生效；
// glue ufsAdapter 经接口断言委派到这里）。
func (OSVFS) Chmod(name string, mode fs.FileMode) error {
	p, err := toOS(name)
	if err != nil {
		return err
	}
	return os.Chmod(p, mode)
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

// filesystemRoots 返回设备文件系统 mount 根列表（deviceFileRoots 专用，
// 原生态路径）。windows：盘符根（C:、D:…）+ 虚拟挂载根 "/"。
func filesystemRoots() []string {
	if runtime.GOOS == "windows" {
		return append(windowsDrives(), "/")
	}
	return []string{"/"}
}
