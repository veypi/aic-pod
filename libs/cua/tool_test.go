package cua

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
	"github.com/veypi/vsh/commands"
)

// runVsh 以 vsh 指令形态执行 cua（argv 解析 + 输出契约）。
func runVsh(t *testing.T, s *Service, args ...string) (string, string, error) {
	t.Helper()
	caller := wire.Caller{Subject: "owner", ConnectionID: "rtc", Origin: "s1", ExpiresAt: time.Now().Add(time.Minute)}
	cmd := s.VshCommand(func(context.Context) wire.Caller { return caller })
	var out, errBuf bytes.Buffer
	inv := &commands.Invocation{Args: args, Env: map[string]string{}, Stdout: &out, Stderr: &errBuf}
	err := commands.RunCommand(context.Background(), cmd, inv)
	return out.String(), errBuf.String(), err
}

func TestVshCommandStrictArgsAndJSONContract(t *testing.T) {
	driver, _ := fakeCuaMcp(t, false)
	s := &Service{driver: driver, native: newNativeUI()}
	// 正常调用：stdout 是 JSON。
	out, _, err := runVsh(t, s, "app.list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &v); err != nil {
		t.Fatalf("stdout not pure JSON: %q (%v)", out, err)
	}
	// 缺位置参数。
	if _, _, err := runVsh(t, s, "window.click"); err == nil {
		t.Fatal("missing window_id admitted")
	}
	// 未知子命令。
	if _, _, err := runVsh(t, s, "window.explode", "w_1"); err == nil {
		t.Fatal("unknown subcommand admitted")
	}
	// wait 拒绝 ref 形态（需要语义 locator + state）。
	if _, _, err := runVsh(t, s, "window.wait", "w_1", "--ref", "old", "--state", "visible"); err == nil {
		t.Fatal("ref-based wait admitted")
	}
	// drag 缺坐标。
	if _, _, err := runVsh(t, s, "window.drag", "w_1", "--snapshot", "s", "--from", "1,2", "--to", "1"); err == nil {
		t.Fatal("bad drag admitted")
	}
	// help 自答。
	out, _, err = runVsh(t, s)
	if err != nil || !strings.Contains(out, "usage: cua") {
		t.Fatalf("help: %q, %v", out, err)
	}
}
