package host

import (
	"os"
	"runtime"
	"strconv"
	"testing"
)

func TestHostIdentityBaseEnv(t *testing.T) {
	env := hostIdentityBaseEnv()
	if runtime.GOOS == "windows" {
		// Windows 无 unix 身份（Getuid=-1），不注入，判定走 owner=current 回落。
		if len(env) != 0 {
			t.Fatalf("windows should not inject identity, got %v", env)
		}
		return
	}
	uid := strconv.Itoa(os.Getuid())
	gid := strconv.Itoa(os.Getgid())
	if env["UID"] != uid || env["EUID"] != uid {
		t.Fatalf("UID/EUID = %q/%q, want %q", env["UID"], env["EUID"], uid)
	}
	if env["GID"] != gid || env["EGID"] != gid {
		t.Fatalf("GID/EGID = %q/%q, want %q", env["GID"], env["EGID"], gid)
	}
}
