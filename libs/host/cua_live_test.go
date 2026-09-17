package host

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/aic-pod/libs/uiscript"
	"github.com/veypi/aic-pod/protocol/ui"
)

// Opt-in, controlled Cocoa fixture only. No existing user window is selected.
func TestNativeUILive(t *testing.T) {
	fixture := os.Getenv("AIC_UI_NATIVE_FIXTURE")
	if fixture == "" {
		t.Skip("set AIC_UI_NATIVE_FIXTURE to the compiled testdata/ui_fixture.swift executable")
	}
	root := t.TempDir()
	output := filepath.Join(root, "state.json")
	cmd := exec.Command(fixture, output)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(output); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture startup timeout")
		}
		time.Sleep(20 * time.Millisecond)
	}
	c := New(Options{WorkDir: root, OnLog: t.Logf})
	c.uiSessionRoot = root
	c.uiScriptExec = []string{os.Args[0], "-test.run=^TestUIScriptWorkerHelper$", "--", uiscript.WorkerArg}
	cuaRt = newCuaMcp(findCuaDriver(), t.Logf)
	defer func() { cuaRt.mu.Lock(); defer cuaRt.mu.Unlock(); cuaRt.killLocked() }()
	cuaUI = newNativeUI()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	request := func(argv ...string) *ui.Result {
		req := &proto.ToolRequest{MsgID: "live", GrantedLevel: 3}
		response := c.runCua(ctx, "native-live", req, append(argv, "--format", "json", "--timeout", "30s"))
		var r ui.Result
		if err := json.Unmarshal([]byte(response.Content), &r); err != nil {
			t.Fatal(err, response.Content)
		}
		return &r
	}
	run := func(argv ...string) *ui.Result {
		r := request(argv...)
		if r.State != "completed" {
			t.Fatalf("%+v", r)
		}
		return r
	}
	list := run("target", "list", "--pid", strconv.Itoa(cmd.Process.Pid))
	b, _ := json.Marshal(list.Data)
	var targets []map[string]any
	json.Unmarshal(b, &targets)
	var target string
	for _, item := range targets {
		if item["title"] == "AIC UI protocol fixture" {
			target = item["id"].(string)
		}
	}
	if target == "" {
		t.Fatalf("fixture window missing: %s", b)
	}
	run("target", "use", target)
	run("fill", "--label", "Fixture name", "--text", "中文 native protocol")
	run("click", "--role", "button", "--name", "Fixture save")
	deadline = time.Now().Add(2 * time.Second)
	for {
		b, _ = os.ReadFile(output)
		var state map[string]any
		json.Unmarshal(b, &state)
		if state["value"] == "中文 native protocol" && state["clicks"] == float64(1) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("independent application state mismatch: %s", b)
		}
		time.Sleep(20 * time.Millisecond)
	}
	shot := run("screenshot")
	if shot.Observation["image"] == nil {
		t.Fatal("missing screenshot geometry")
	}
	// The same SDK now drives the native adapter through the real host entry.
	payload, _ := json.Marshal(map[string]any{"action": "cua", "argv": []string{"run", "--code", `await ui.fill({label:'Fixture name'},'批量原生脚本');await ui.click({role:'button',name:'Fixture save'});return 'native batch done';`, "--after", "screenshot", "--format", "json"}})
	batch := c.execCmd(ctx, "native-live", &proto.ToolRequest{Tool: proto.ToolExec, MsgID: "native-script", GrantedLevel: 3, Data: payload})
	if batch.State != proto.StateCompleted || batch.Attrs["image_data"] == "" {
		t.Fatal(batch.Content, batch.Error)
	}
	deadline = time.Now().Add(2 * time.Second)
	for {
		b, _ = os.ReadFile(output)
		var state map[string]any
		json.Unmarshal(b, &state)
		if state["value"] == "批量原生脚本" && state["clicks"] == float64(2) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("native batch application state mismatch: %s", b)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// End only this test process's implicit MCP session. Recovery must happen
	// on the same child, and must invalidate old target/ref bindings.
	pid := cuaRt.cmd.Process.Pid
	if _, err := cuaRt.call(ctx, "end_session", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	expired := request("apps")
	if expired.Error == nil || expired.Error.Code != "session_expired" {
		t.Fatalf("expected expired implicit session: %+v", expired)
	}
	run("apps")
	if cuaRt.cmd.Process.Pid != pid {
		t.Fatal("session recovery restarted the MCP child")
	}
	old := request("target", "use", target)
	if old.Error == nil || old.Error.Code != "target_closed" {
		t.Fatalf("expired target reused: %+v", old)
	}
	run("target", "list", "--pid", strconv.Itoa(cmd.Process.Pid))
	t.Log("ended implicit session recovered on the same MCP child; old targets rejected")
	if err := cuaUI.endSession(ctx, "native-live"); err != nil {
		t.Fatal(err)
	}
}
