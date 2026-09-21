package cua

import (
	"context"
	"fmt"
	"testing"

	"github.com/veypi/aic-pod/protocol/ui"
)

func TestNativeUIBindingsAndDelivery(t *testing.T) {
	e := newNativeUI()
	e.identity = func(context.Context, int) (string, error) { return "birth-one", nil }
	root := t.TempDir()
	calls := []string{}
	observeID := 0
	failObserve := false
	call := func(_ context.Context, name string, args map[string]any) (*mcpResult, error) {
		calls = append(calls, name)
		switch name {
		case "list_windows":
			return &mcpResult{StructuredContent: map[string]any{"windows": []any{map[string]any{"pid": 10, "window_id": 20, "app_name": "Test", "title": "Fixture", "bounds": map[string]any{"x": 0, "y": 0, "width": 100, "height": 50}}}}}, nil
		case "get_window_state":
			if failObserve {
				return nil, fmt.Errorf("capture failed")
			}
			observeID++
			return &mcpResult{StructuredContent: map[string]any{"elements": []any{map[string]any{"role": "AXButton", "label": "Save", "element_token": fmt.Sprint(observeID), "element_index": 1}}, "element_count": 1}}, nil
		case "double_click":
			if args["count"] != nil || args["button"] != nil {
				t.Fatal("unexpected driver arguments")
			}
		case "bring_to_front":
			if args["session"] != nil {
				t.Fatal("unexpected session argument")
			}
		}
		return &mcpResult{StructuredContent: map[string]any{"status": "ok"}}, nil
	}
	run := func(sid string, epoch uint64, argv ...string) *ui.Result {
		o, err := ui.Parse("cua", argv)
		if err != nil {
			t.Fatal(err)
		}
		return e.execute(context.Background(), o, sid, root, epoch, call)
	}
	list := run("one", 1, "target", "list")
	target := list.Data.([]map[string]any)[0]["id"].(string)
	first := run("one", 1, "snapshot", "--target", target)
	ref := first.Observation["elements"].([]map[string]any)[0]["ref"].(string)
	r := run("one", 1, "click", ref, "--target", target, "--count", "2", "--after", "none")
	if r.State != "completed" {
		t.Fatalf("%+v", r)
	}
	r = run("one", 1, "click", ref, "--target", target)
	if r.Error == nil || r.Error.Code != "stale_ref" {
		t.Fatalf("old ref accepted: %+v", r)
	}
	fresh := run("one", 1, "snapshot", "--target", target)
	ref = fresh.Observation["elements"].([]map[string]any)[0]["ref"].(string)
	run("two", 1, "target", "list")
	r = run("two", 1, "snapshot", "--target", target)
	if r.Error == nil || r.Error.Code != "target_closed" {
		t.Fatal("target crossed session")
	}
	failObserve = true
	r = run("one", 1, "click", ref, "--target", target)
	if r.State != "completed" || r.Action["performed"] != true || len(r.Warnings) == 0 {
		t.Fatalf("observation failure erased success: %+v", r)
	}
	failObserve = false
	r = run("one", 2, "snapshot", "--target", target)
	if r.Error == nil || r.Error.Code != "target_closed" {
		t.Fatal("runtime restart retained binding")
	}
}
func TestNativeWebAncestry(t *testing.T) {
	e := newNativeUI()
	web := &nativeElement{raw: map[string]any{"element_index": 1, "role": "AXWebArea"}}
	field := &nativeElement{raw: map[string]any{"element_index": 2, "parent_index": 1, "role": "AXTextField"}}
	target := &nativeTarget{snapshot: &nativeSnapshot{order: []*nativeElement{web, field}}}
	if !e.webElement(target, field) {
		t.Fatal("web field trusted as native")
	}
}

func TestNativeQueryMatchesOnlyVisibleValues(t *testing.T) {
	element := map[string]any{"name": "保存", "role": "button", "value": 123, "ref": "@s123:e1", "enabled": true, "native_role": "AXButton"}
	for _, query := range []string{"保存", "BUTTON", "123"} {
		if !nativeQueryMatch(element, query) {
			t.Fatalf("visible value %q did not match", query)
		}
	}
	for _, query := range []string{"name", "enabled", "native_role", "AXButton", "@s123"} {
		if nativeQueryMatch(element, query) {
			t.Fatalf("metadata %q matched", query)
		}
	}
}
