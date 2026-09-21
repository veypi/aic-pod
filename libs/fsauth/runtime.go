package fsauth

import (
	"os"
	"path/filepath"
	"runtime"
)

// RuntimeReadRoots are explicit read-only resources needed to start local tools.
func RuntimeReadRoots() []string {
	switch runtime.GOOS {
	case "darwin":
		// /etc → /private/etc、/var/select → /private/var/select 是系统 symlink：
		// 沙箱按字面路径串匹配规则，两种拼写都要在名单里（2026-09-22）。
		roots := []string{canonical("/System/Library"), canonical("/System/Cryptexes/OS/System/Library"), canonical("/System/Cryptexes/OS/usr"), canonical("/usr"), canonical("/bin"), canonical("/sbin"), canonical("/Library/Apple"), canonical("/private/etc"), "/etc", canonical("/private/var/select"), "/var/select", canonical("/dev")}
		return append(roots, xcodeToolchainRoots()...)
	case "linux":
		return []string{canonical("/usr"), canonical("/bin"), canonical("/sbin"), canonical("/lib"), canonical("/lib64"), canonical("/etc"), canonical("/dev"), canonical("/proc")}
	case "windows":
		return []string{canonical(os.Getenv("SystemRoot")), canonical(filepath.Join(os.Getenv("SystemRoot"), "System32"))}
	}
	return nil
}

// xcodeToolchainRoots 是 Xcode 命令行工具 shim 启动所需的只读根：
// /usr/bin/git、python3、cc 等 shim 先读 select link 解析 developer dir，
// 再去那里执行真实工具；缺读权限时 shim 直接失败
// （xcode-select: error: unable to read data link at
// '/var/db/xcode_select_link', expected symbolic link (Operation not permitted)，
// 2026-09-22 收紧读锁后复测发现）。只收录实际存在的路径。
func xcodeToolchainRoots() []string {
	const link = "/var/db/xcode_select_link"
	var roots []string
	add := func(p string) { roots = append(roots, dualForms(p)...) }
	add(link)
	if target, err := os.Readlink(link); err == nil && target != "" {
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(link), target)
		}
		add(target)
	}
	for _, dir := range []string{"/Library/Developer/CommandLineTools", "/Applications/Xcode.app/Contents/Developer"} {
		if _, err := os.Stat(dir); err == nil {
			add(dir)
		}
	}
	return dedupClean(roots)
}
