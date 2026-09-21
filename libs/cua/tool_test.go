package cua

import (
	"context"
	"encoding/json"
	tool "github.com/veypi/aic-pod/libs/hosts_tool"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
	"testing"
	"time"
)

func TestTypedNativeRegistrationAndStrictArgs(t *testing.T) {
	driver, _ := fakeCuaMcp(t, false)
	s := &Service{driver: driver, native: newNativeUI()}
	d := tool.New(tool.Config{})
	if err := d.RegisterCommand(s.Tool()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close(context.Background()) })
	caller := tool.Caller{Subject: "owner", ConnectionID: "rtc", Level: 3, ExpiresAt: time.Now().Add(time.Minute)}
	invoke := func(method string, args any) wire.Response {
		raw, _ := json.Marshal(args)
		return d.Handle(context.Background(), caller, wire.Request{Protocol: "hosts_tools/1", ID: wire.NewID("r_"), Action: "call", Call: &wire.Invocation{Domain: "exec", Command: "cua", Method: method, Args: raw}})
	}
	if r := invoke("app.list", map[string]any{}); r.Error != nil {
		t.Fatal(r.Error)
	}
	for _, args := range []map[string]any{{"locator": map[string]any{"ref": "@old"}}, {"window_id": "w_1", "locator": map[string]any{"role": "button", "ref": "@old"}}, {"window_id": "w_1", "locator": map[string]any{"role": "button"}, "session_id": "legacy"}} {
		if r := invoke("window.click", args); r.Error == nil || r.Error.Code != "invalid_argument" {
			t.Fatalf("bad args admitted: %+v", r)
		}
	}
	if r := invoke("window.drag", map[string]any{"window_id": "w_1", "snapshot": "s", "from_at": []int{1, 2}, "to_at": []int{1}}); r.Error == nil {
		t.Fatal("bad drag admitted")
	}
	if r := invoke("window.wait", map[string]any{"window_id": "w_1", "locator": map[string]any{"ref": "old"}, "state": "visible"}); r.Error == nil {
		t.Fatal("ref-based wait admitted")
	}
	caller.Level = 1
	if r := invoke("clipboard.write", map[string]any{"text": "abc"}); r.Error == nil || r.Error.Code != "permission_denied" {
		t.Fatalf("write grant bypass: %+v", r)
	}
}
