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
		return []string{canonical("/System/Library"), canonical("/System/Cryptexes/OS/System/Library"), canonical("/System/Cryptexes/OS/usr"), canonical("/usr"), canonical("/bin"), canonical("/sbin"), canonical("/Library/Apple"), canonical("/private/etc"), canonical("/private/var/select"), canonical("/dev")}
	case "linux":
		return []string{canonical("/usr"), canonical("/bin"), canonical("/sbin"), canonical("/lib"), canonical("/lib64"), canonical("/etc"), canonical("/dev"), canonical("/proc")}
	case "windows":
		return []string{canonical(os.Getenv("SystemRoot")), canonical(filepath.Join(os.Getenv("SystemRoot"), "System32"))}
	}
	return nil
}
