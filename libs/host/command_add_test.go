package host

import (
	"context"
	"testing"

	"github.com/veypi/aic-pod/protocol"
)

// commands add 的回执必须反映命令面真相：撞平台命令名（rejected）与被同名
// 技能命令覆盖（active 里是别人的定义）都不能回 "added" + exit 0。
func TestUnifiedCommandsAddReceipt(t *testing.T) {
	c, _ := testClient(t)
	c.perms.grantCmd("s1", "commands")
	run := func(script string) *protocol.Output {
		t.Helper()
		response := callTool(t, c, context.Background(), testCaller(), execRequest(script, 30000))
		result := decoded[protocol.Output](t, response.Result)
		return &result
	}

	if out := run("commands add receipt-ok -- /bin/echo hi"); out.Attrs["exit_code"] != "0" {
		t.Fatalf("valid add failed: %+v", out)
	}
	if !c.cmds.has("receipt-ok") {
		t.Fatal("receipt-ok not active after add")
	}

	// 撞平台内建命令名：定义进 rejected，回执必须非零。
	if out := run("commands add commands -- /bin/echo hi"); out.Attrs["exit_code"] == "0" {
		t.Fatalf("add colliding with platform command reported success: %+v", out)
	}
	if c.cmds.rejection("commands") == "" {
		t.Fatal("platform name collision not recorded in rejected")
	}

	// 与预装技能命令同名：config 源被技能定义覆盖，回执同样必须非零。
	if out := run("commands add browser -- /bin/echo hi"); out.Attrs["exit_code"] == "0" {
		t.Fatalf("add shadowed by skill command reported success: %+v", out)
	}

	// 远程端点没有进程：cwd/env 显式拒绝，不静默丢弃。
	for _, script := range []string{
		"commands add bad-remote --protocol mcp --url http://127.0.0.1:9000/mcp --cwd /tmp",
		"commands add bad-remote --protocol mcp --url http://127.0.0.1:9000/mcp --env A=B",
	} {
		if out := run(script); out.Attrs["exit_code"] == "0" {
			t.Fatalf("url command with process settings was accepted: %s", script)
		}
	}
}
