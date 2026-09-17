package uiscript

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/veypi/aic-pod/protocol/ui"
)

func runWorker(t *testing.T, domain, code string, results ...*ui.Result) []Message {
	t.Helper()
	var input, output bytes.Buffer
	enc := json.NewEncoder(&input)
	enc.Encode(Init{Domain: domain, Code: code})
	for i, r := range results {
		enc.Encode(map[string]any{"index": i + 1, "result": r})
	}
	if err := Serve(&input, &output); err != nil {
		t.Fatal(err)
	}
	var messages []Message
	for _, line := range bytes.Split(bytes.TrimSpace(output.Bytes()), []byte{'\n'}) {
		var m Message
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatal(err, string(line))
		}
		messages = append(messages, m)
	}
	return messages
}
func success(op string) *ui.Result {
	return &ui.Result{Protocol: "ui/1", Domain: "browser", Op: op, State: "completed"}
}
func TestSDKUsesUIProtocolAndFreshRefs(t *testing.T) {
	snapshot := success("snapshot")
	snapshot.Observation = map[string]any{"snapshot": "sfirst", "elements": []any{map[string]any{"ref": "@sfirst:e1", "role": "textbox", "name": "邮箱"}}}
	filled := success("fill")
	filled.Observation = map[string]any{"snapshot": "snext", "elements": []any{map[string]any{"ref": "@snext:e1", "role": "button", "name": "保存"}}}
	for _, domain := range []string{"browser", "cua"} {
		messages := runWorker(t, domain, `const s=await ui.snapshot(); const r=await ui.fill(s.ref({role:'textbox',name:'邮箱'}),'中文\n"值"'); await ui.click(r.observation.ref({role:'button',name:'保存'})); return r;`, snapshot, filled, success("click"))
		if len(messages) != 4 {
			t.Fatalf("%+v", messages)
		}
		for i, want := range [][]string{{"snapshot"}, {"fill", "--text", "中文\n\"值\"", "--", "@sfirst:e1"}, {"click", "--", "@snext:e1"}} {
			if !reflect.DeepEqual(messages[i].Argv, want) {
				t.Fatalf("want %q got %q", want, messages[i].Argv)
			}
		}
		if messages[3].Error != nil || bytes.Contains(messages[3].Value, []byte(`"ref":{`)) {
			t.Fatalf("%+v", messages[3])
		}
	}
}
func TestSDKExplicitDialogRecovery(t *testing.T) {
	modal := success("click")
	modal.Fail(ui.Err("dialog_open", "sync confirm"), "unknown")
	ms := runWorker(t, "browser", `try {await ui.click({role:'button',name:'Go'});} catch(e) {if(e.code!=='dialog_open'||e.step!==1)throw e;await ui.dialog.dismiss();} return 'recovered';`, modal, success("dialog.dismiss"))
	if len(ms) != 3 || ms[2].Error != nil || string(ms[2].Value) != `"recovered"` {
		t.Fatalf("%+v", ms)
	}
}
func TestSDKStopsAndRejectsInvalidAsyncUsage(t *testing.T) {
	for _, tc := range []struct{ code, want string }{
		{`await ui.run({code:'x'})`, "script_error"},
		{`await ui.command('run',{code:'x'})`, "unsupported"},
		{`ui.click({role:'button'});return 1`, "unawaited_call"},
		{`await Promise.all([ui.click({role:'button'}),ui.click({role:'button'})])`, "concurrent_call"},
		{`await new Promise(()=>{})`, "unresolved_promise"},
		{`return (()=>{const a={};a.a=a;return a})()`, "invalid_result"},
	} {
		ms := runWorker(t, "browser", tc.code)
		last := ms[len(ms)-1]
		if last.Error == nil || last.Error.Code != tc.want || len(ms) != 1 {
			t.Fatalf("%s: %+v", tc.code, ms)
		}
	}
	failure := success("click")
	failure.Fail(ui.Err("stale_ref", "old ref"), false)
	ms := runWorker(t, "browser", `await ui.click('@sold:e1');await ui.click('@sold:e2');`, failure)
	if len(ms) != 2 || ms[1].Error.Code != "stale_ref" || ms[1].Step != 1 {
		t.Fatalf("%+v", ms)
	}
}
func TestSDKNoHostObjectsAndUniqueRefs(t *testing.T) {
	ms := runWorker(t, "cua", `return [typeof process,typeof require,typeof fetch,typeof global,typeof __bridge,typeof setTimeout];`)
	if string(ms[0].Value) != `["undefined","undefined","undefined","undefined","undefined","undefined"]` {
		t.Fatalf("%s", ms[0].Value)
	}
	snapshot := success("snapshot")
	snapshot.Observation = map[string]any{"elements": []any{map[string]any{"role": "button", "name": "Save", "ref": "@s1:e1"}, map[string]any{"role": "button", "name": "Save", "ref": "@s1:e2"}}}
	ms = runWorker(t, "browser", `const s=await ui.snapshot(); await ui.click(s.ref({name:'Save'}));`, snapshot)
	if len(ms) != 2 || ms[1].Error.Code != "ambiguous_target" {
		t.Fatalf("%+v", ms)
	}
}
