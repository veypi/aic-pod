package host

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/veypi/aic-pod/cfg"
)

// writeState 落盘 {UserConfigDir}/aic/state.json（HOME 重定向到临时目录），
// 产物可被桌面按 pid 核对消费；tmp 文件必须已被 rename 清掉。
func TestWriteStateRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeState(State{Connected: false, HostID: "abc", LastError: "nats: Authorization Violation", Retrying: true})
	dir, err := cfg.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "state.json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("state.json not written: %v", err)
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if s.PID != os.Getpid() || s.Connected || !s.Retrying || s.HostID != "abc" || s.UpdatedAt == "" {
		t.Fatalf("state roundtrip mismatch: %+v", s)
	}
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("tmp file must be renamed away")
	}
}

func TestHostIDOf(t *testing.T) {
	if got := hostIDOf("abc.2.sec.uid"); got != "abc" {
		t.Fatalf("hostIDOf = %q", got)
	}
	if got := hostIDOf(""); got != "" {
		t.Fatalf("hostIDOf empty = %q", got)
	}
	if got := hostIDOf("broken.key"); got != "" {
		t.Fatalf("hostIDOf malformed = %q", got)
	}
}
