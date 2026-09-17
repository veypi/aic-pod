package host

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/aic-pod/libs/uiscript"
	"github.com/veypi/aic-pod/protocol/ui"
)

func TestUIScriptWorkerHelper(t *testing.T) {
	if os.Args[len(os.Args)-1] != uiscript.WorkerArg {
		t.Skip("helper only")
	}
	os.Exit(uiscript.Main())
}
func scriptClient(t *testing.T) (*Client, *[]string) {
	t.Helper()
	c, _ := testClient(t)
	c.uiScriptExec = []string{os.Args[0], "-test.run=^TestUIScriptWorkerHelper$", "--", uiscript.WorkerArg}
	calls := []string{}
	provider := Provider{Decl: proto.CommandDecl{Name: "browser", RequiredLevel: 1}, Run: func(ctx context.Context, sid string, req *proto.ToolRequest, argv []string) *proto.ToolResponse {
		o, err := ui.Parse("browser", argv)
		if err != nil {
			t.Fatal(err)
		}
		calls = append(calls, o.Op)
		r := ui.NewResult(o)
		r.Target = map[string]any{"id": "tfixture", "kind": "tab"}
		switch o.Op {
		case "click":
			r.Action = map[string]any{"performed": true}
		case "fill":
			if o.String("text") == "fail" {
				r.Fail(ui.Err("not_actionable", "fixture failure"), false)
			} else {
				r.Action = map[string]any{"performed": true}
			}
		case "get":
			r.Data = map[string]any{"value": "中文"}
		case "wait":
			<-ctx.Done()
			r.Fail(ui.Err("timeout", ctx.Err().Error()), false)
		case "snapshot", "screenshot", "target.use":
			r.Observation = map[string]any{"snapshot": "sfixture", "elements": []any{map[string]any{"ref": "@sfixture:e1", "role": "button", "name": "Save"}}, "text": "@sfixture:e1 button Save"}
			if o.Op == "screenshot" {
				r.Images = map[string]string{"image_data": "data:image/png;base64,eA=="}
				r.Artifacts = []map[string]any{{"kind": "image", "path": "fixture.png"}}
			}
		}
		return uiResponse(req.MsgID, o, r, c.uiWorkDir(sid))
	}}
	providersMu.Lock()
	previous, existed := providers["browser"]
	providers["browser"] = provider
	providersMu.Unlock()
	t.Cleanup(func() {
		providersMu.Lock()
		defer providersMu.Unlock()
		if existed {
			providers["browser"] = previous
		} else {
			delete(providers, "browser")
		}
	})
	c.addProvider(provider.Decl)
	// The production worker fails closed when this platform cannot implement
	// the configured OS policy. Pure SDK tests still run on those platforms.
	probe, _ := runScript(t, c, []string{"run", "--code", "return null;", "--after", "none"}, 3)
	if probe.Error != nil && probe.Error.Code == "script_unavailable" {
		t.Skip(probe.Error.Message)
	}
	if probe.State != "completed" {
		t.Fatalf("script worker probe: %+v", probe)
	}
	return c, &calls
}
func runScript(t *testing.T, c *Client, argv []string, level int) (*ui.Result, *proto.ToolResponse) {
	t.Helper()
	request := signedReq(t, c, proto.ToolExec, map[string]any{"action": "browser", "argv": append(argv, "--format", "json")}, level)
	response := c.dispatch(context.Background(), testSubject, request)
	var result ui.Result
	if err := json.Unmarshal([]byte(response.Content), &result); err != nil {
		t.Fatal(err, response.Content, response.Error)
	}
	return &result, response
}
func TestUIScriptStepsFileAndFinalImage(t *testing.T) {
	c, calls := scriptClient(t)
	file := filepath.Join(c.opts.WorkDir, "script.js")
	if err := os.WriteFile(file, []byte(`const s=await ui.snapshot();for(let i=0;i<2;i++)await ui.click({role:'button',name:'Save'});console.log('中文 log');return (await ui.get('value',{label:'Name'})).data.value;`), 0600); err != nil {
		t.Fatal(err)
	}
	r, response := runScript(t, c, []string{"run", "--file", file, "--after", "screenshot"}, 3)
	if r.State != "completed" {
		t.Fatalf("%s", response.Content)
	}
	d := r.Data.(map[string]any)
	if d["return"] != "中文" || d["steps_completed"] != float64(4) || len(d["logs"].([]any)) != 1 {
		t.Fatalf("%#v", d)
	}
	if strings.Join(*calls, ",") != "snapshot,click,click,get,screenshot" {
		t.Fatal(*calls)
	}
	if response.Attrs["image_data"] == "" || r.Action["performed"] != true {
		t.Fatal(response)
	}
}
func TestUIScriptFailurePermissionAndTimeout(t *testing.T) {
	c, calls := scriptClient(t)
	r, _ := runScript(t, c, []string{"run", "--code", `await ui.click({role:'button'});await ui.fill({label:'Name'},'fail');await ui.click({role:'button'});`}, 3)
	if r.Error == nil || r.Error.Code != "not_actionable" || r.Action["performed"] != true || len(*calls) != 2 {
		t.Fatalf("%+v calls=%v", r, *calls)
	}
	if r.Data.(map[string]any)["failed_step"] != float64(2) {
		t.Fatal(r.Data)
	}
	r, _ = runScript(t, c, []string{"run", "--code", `await ui.click({role:'button'});`}, 2)
	if r.State != "rejected" || len(*calls) != 2 {
		t.Fatalf("%+v calls=%v", r, *calls)
	}
	start := time.Now()
	r, _ = runScript(t, c, []string{"run", "--timeout", "300ms", "--code", `await ui.click({role:'button'});for(;;){}`}, 3)
	if r.Error == nil || r.Error.Code != "timeout" || r.Action["performed"] != true || len(*calls) != 3 || time.Since(start) > 3*time.Second {
		t.Fatalf("%+v calls=%v", r, *calls)
	}
	r, _ = runScript(t, c, []string{"run", "--after", "none", "--code", `return 42;`}, 3)
	if r.State != "completed" || r.Data.(map[string]any)["return"] != float64(42) {
		t.Fatalf("worker did not recover: %+v", r)
	}
	r, _ = runScript(t, c, []string{"run", "--timeout", "300ms", "--code", `await ui.wait({ms:1000});await ui.click({role:'button'});`}, 3)
	if r.Error == nil || r.Error.Code != "timeout" || len(*calls) != 4 || (*calls)[3] != "wait" || r.Data.(map[string]any)["failed_step"] != float64(1) {
		t.Fatalf("step timeout lost its result: %+v calls=%v", r, *calls)
	}
}
func TestUIScriptDoesNotReplayOrBypassStepValidation(t *testing.T) {
	c, calls := scriptClient(t)
	body := signedReq(t, c, proto.ToolExec, map[string]any{"action": "browser", "argv": []string{"run", "--code", `await ui.click({role:'button'});throw Error('stop');`, "--format", "json"}}, 3)
	first := c.dispatch(context.Background(), testSubject, body)
	var req proto.ToolRequest
	json.Unmarshal(body, &req)
	req.Nonce, _ = proto.NewNonce()
	proto.SignToolRequest(&req, c.hostID, c.kTool)
	body, _ = json.Marshal(req)
	second := c.dispatch(context.Background(), testSubject, body)
	if first.Content != second.Content || len(*calls) != 1 {
		t.Fatalf("batch replayed: %v", *calls)
	}
	for _, code := range []string{`await ui.call(['run','--code','return 1']);`, `await ui.call(['click','--no-such-option']);`, `await ui.command('exec',{code:'sh'});`} {
		r, _ := runScript(t, c, []string{"run", "--code", code}, 3)
		if r.State != "error" {
			t.Fatalf("%+v", r)
		}
	}
	if len(*calls) != 1 {
		t.Fatalf("invalid step reached provider: %v", *calls)
	}
}
func TestUIScriptFileAndUploadUseHostPolicy(t *testing.T) {
	c, calls := scriptClient(t)
	file := filepath.Join(c.opts.WorkDir, "denied-script.js")
	os.WriteFile(file, []byte(`return 1;`), 0600)
	previous := cfg.Global.FsDeny
	cfg.Global.FsDeny = append(append([]string{}, previous...), file)
	c.policy.Reconcile()
	t.Cleanup(func() { cfg.Global.FsDeny = previous })
	r, _ := runScript(t, c, []string{"run", "--file", file}, 3)
	if r.State != "rejected" {
		t.Fatalf("%+v", r)
	}
	code := `await ui.upload({label:'File'},` + strconvQuote(file) + `);`
	r, _ = runScript(t, c, []string{"run", "--code", code}, 3)
	if r.State != "error" || len(*calls) != 0 {
		t.Fatalf("upload escaped policy: %+v %v", r, *calls)
	}
}
func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestUIScriptLimitsPreservePartialSteps(t *testing.T) {
	c, calls := scriptClient(t)
	r, _ := runScript(t, c, []string{"run", "--timeout", "10s", "--after", "none", "--code", `await ui.click({role:'button'});const chunks=[];for(;;)chunks.push(new ArrayBuffer(1024*1024));`}, 3)
	if r.Error == nil || r.Error.Code != "resource_limit" || r.Action["performed"] != true || len(*calls) != 1 {
		t.Fatalf("%+v calls=%v", r, *calls)
	}
	r, _ = runScript(t, c, []string{"run", "--after", "none", "--code", `for(let i=0;i<257;i++)await ui.get('value',{label:'Name'});`}, 3)
	if r.Error == nil || r.Error.Code != "resource_limit" || r.Data.(map[string]any)["steps_completed"] != float64(256) || r.Data.(map[string]any)["failed_step"] != float64(257) {
		t.Fatalf("%+v", r)
	}
	if len(*calls) != 257 {
		t.Fatalf("extra step dispatched: %d", len(*calls))
	}
}
