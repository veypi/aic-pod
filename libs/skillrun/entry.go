package skillrun

import (
	"os"
	"path/filepath"
	"runtime"
)

// resolveEntry 把 manifest 里的 entry 解析成包内实际路径。
//
// entry 是平台中立的相对路径（如 browser 的 "cli/bin/browser-service"）；而 provider
// 构建产物在 Windows 上带 .exe——无扩展名时 CreateProcess 会自动补 .exe、
// exec.LookPath 也按 PATHEXT 查找，两者都找不到文件（2026-10-05 win 实机：
// "exec: unknown action <path>"）。因此 Windows 上 **.exe 形态优先**（旧安装残留的
// 无后缀同名文件不可执行，不能让它胜出），其余平台严格按原样（宁缺毋滥，不掩盖清单写错）。
func resolveEntry(dir, entry string) (string, error) {
	return resolveEntryFor(dir, entry, runtime.GOOS)
}

func resolveEntryFor(dir, entry, goos string) (string, error) {
	path := filepath.Join(dir, filepath.FromSlash(entry))
	if goos == "windows" {
		if alt := path + ".exe"; exists(alt) {
			return alt, nil
		}
	}
	if exists(path) {
		return path, nil
	}
	return "", &os.PathError{Op: "stat", Path: path, Err: os.ErrNotExist}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
