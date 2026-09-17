package host

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/aic-pod/libs/uiscript"
	"github.com/veypi/aic-pod/protocol/ui"
)

// Invoked by desktop/scripts/test-browser-live.mjs against its private provider.
func TestBrowserScriptLive(t *testing.T) {
	address := os.Getenv("AIC_UI_TEST_BROWSER_ADDR")
	if address == "" {
		t.Skip("requires the owned Electron fixture provider")
	}
	c := New(Options{WorkDir: os.TempDir(), OnLog: t.Logf})
	c.uiScriptExec = []string{os.Args[0], "-test.run=^TestUIScriptWorkerHelper$", "--", uiscript.WorkerArg}
	provider := Provider{Decl: proto.CommandDecl{Name: "browser", RequiredLevel: 1}, Run: RunViaShell(ShellChannel{Addr: address, Token: os.Getenv("AIC_UI_TEST_BROWSER_TOKEN")})}
	providersMu.Lock()
	providers["browser"] = provider
	providersMu.Unlock()
	c.addProvider(provider.Decl)
	code := `await ui.fill({label:'Email'},'批量脚本@example.test');
try {await ui.click({role:'button',name:'Confirm'});} catch(e) {if(e.code!=='dialog_open')throw e;await ui.dialog.accept();}
const s=await ui.snapshot({query:'Email'});
return (await ui.get('value',s.ref({role:'textbox',name:'Email'}))).data.value;`
	data, _ := json.Marshal(map[string]any{"action": "browser", "argv": []string{"run", "--target", os.Getenv("AIC_UI_TEST_BROWSER_TARGET"), "--code", code, "--after", "screenshot", "--format", "json"}})
	response := c.execCmd(context.Background(), os.Getenv("AIC_UI_TEST_BROWSER_SID"), &proto.ToolRequest{Tool: proto.ToolExec, MsgID: "script-live", GrantedLevel: 3, Deadline: time.Now().Add(30 * time.Second).Format(time.RFC3339), Data: data})
	var r ui.Result
	if json.Unmarshal([]byte(response.Content), &r) != nil || r.State != "completed" {
		t.Fatal(response.Content, response.Error)
	}
	if r.Action["performed"] != true {
		t.Fatalf("resolved dialog left batch outcome unknown: %+v", r.Action)
	}
	d := r.Data.(map[string]any)
	if d["truncated"] == true {
		full, err := os.ReadFile(d["path"].(string))
		if err != nil {
			t.Fatal(err)
		}
		var report ui.Result
		if json.Unmarshal(full, &report) != nil {
			t.Fatal("invalid full script report")
		}
		d = report.Data.(map[string]any)
	}
	if d["return"] != "批量脚本@example.test" {
		t.Fatalf("%+v", d)
	}
	if len(d["steps"].([]any)) != 5 || response.Attrs["image_data"] == "" {
		t.Fatal("missing transcript or final screenshot")
	}
	t.Log("PASS: host JS worker -> browser TCP -> real CDP, including explicit synchronous dialog recovery")
}
