package cua

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// runCLI 以包 CLI 形态执行 cua（argv 解析 + 输出契约），返回 stdout/stderr/退出码。
func runCLI(t *testing.T, s *Service, args ...string) (string, string, int) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := s.Run(context.Background(), args, "", &out, &errBuf)
	return out.String(), errBuf.String(), code
}

func TestCLIStrictArgsAndJSONContract(t *testing.T) {
	driver, _ := fakeCuaMcp(t, false)
	s := &Service{driver: driver, native: newNativeUI()}
	// 正常调用：stdout 是 JSON。
	out, _, code := runCLI(t, s, "app.list", "--json")
	if code != 0 {
		t.Fatalf("app.list: code=%d", code)
	}
	var v any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &v); err != nil {
		t.Fatalf("stdout not pure JSON: %q", out)
	}
	// 缺位置参数。
	if _, _, code := runCLI(t, s, "window.click"); code == 0 {
		t.Fatal("missing window_id admitted")
	}
	// 未知子命令。
	if _, _, code := runCLI(t, s, "window.explode", "w_1"); code == 0 {
		t.Fatal("unknown subcommand admitted")
	}
	// wait 拒绝 ref 形态（需要语义 locator + state）。
	if _, _, code := runCLI(t, s, "window.wait", "w_1", "--ref", "old", "--state", "visible"); code == 0 {
		t.Fatal("ref-based wait admitted")
	}
	// drag 缺坐标。
	if _, _, code := runCLI(t, s, "window.drag", "w_1", "--snapshot", "s", "--from", "1,2", "--to", "1"); code == 0 {
		t.Fatal("bad drag admitted")
	}
	// help 自答。
	out, _, code = runCLI(t, s)
	if code != 0 || !strings.Contains(out, "usage: cua") {
		t.Fatalf("help: %q, code=%d", out, code)
	}
}
