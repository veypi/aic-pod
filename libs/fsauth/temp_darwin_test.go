//go:build darwin

package fsauth

import "testing"

// darwin 临时区字面根：/tmp（symlink → /private/tmp）写入判 (1,2)，
// /private/tmp 在沙箱 bind 白名单（v0.14.5 丢失，2026-09-21 补回）。
func TestTempRootsDarwin(t *testing.T) {
	p := newTestPolicy(t, "")
	// 本用例专门验证真实临时区，恢复隔离 fixture 移除的全局根。
	p.rebuildBaseRootsLocked()
	assertGrades(t, p, "/tmp/fsauth-probe", 1, 2)
	for _, r := range p.WriteRootsFor("s1") {
		if r == "/private/tmp" {
			return
		}
	}
	t.Fatalf("WriteRootsFor missing /private/tmp")
}
