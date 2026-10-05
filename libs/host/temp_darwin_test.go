//go:build darwin

package host

import (
	"testing"

	"github.com/veypi/vbox"
)

// darwin 临时区字面根：/tmp（symlink → /private/tmp）写入判 (1,2)，
// /private/tmp 在沙箱 bind 白名单（v0.14.5 丢失，2026-09-21 补回）。
func TestTempRootsDarwin(t *testing.T) {
	p := newTestPerms(t, "")
	// 本用例专门验证真实临时区，恢复隔离 fixture 移除的全局根。
	p.rebuildRoots()
	rows := p.fsSnapshot("s1").Rules
	if d := p.fsSnapshot("s1").Match("/tmp/fsauth-probe", vbox.OpWrite); !d.Allow {
		t.Fatal("/tmp write denied")
	}
	for _, r := range rows {
		if r.Effect == vbox.EffRW && r.Pattern == "/private/tmp" {
			return
		}
	}
	t.Fatalf("fs snapshot write rules missing /private/tmp: %v", rows)
}
