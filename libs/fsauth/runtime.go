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
		return []string{canonical("/System/Library"), canonical("/System/Cryptexes/OS/System/Library"), canonical("/System/Cryptexes/OS/usr"), canonical("/usr"), canonical("/bin"), canonical("/sbin"), canonical("/Library/Apple"), canonical("/private/etc"), "/etc", canonical("/private/var/select"), "/var/select", canonical("/dev")}
	case "linux":
		return []string{canonical("/usr"), canonical("/bin"), canonical("/sbin"), canonical("/lib"), canonical("/lib64"), canonical("/etc"), canonical("/dev"), canonical("/proc")}
	case "windows":
		return []string{canonical(os.Getenv("SystemRoot")), canonical(filepath.Join(os.Getenv("SystemRoot"), "System32"))}
	}
	return nil
}
