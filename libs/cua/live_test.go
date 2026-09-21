//go:build darwin

package cua

import (
	"context"
	"encoding/json"
	tool "github.com/veypi/aic-pod/libs/hosts_tool"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Explicitly opt in; only the temporary fixture window is operated on.
func TestNativeTypedLive(t *testing.T) {
	bin := os.Getenv("AIC_CUA_FIXTURE")
	if bin == "" {
		t.Skip("set AIC_CUA_FIXTURE to compiled testdata/ui_fixture.swift")
	}
	state := filepath.Join(t.TempDir(), "state.json")
	cmd := exec.Command(bin, state)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	s := New(Config{Logf: t.Logf})
	d := tool.New(tool.Config{})
	if err := d.RegisterCommand(s.Tool()); err != nil {
		t.Fatal(err)
	}
	defer d.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	caller := tool.Caller{Subject: "fixture-owner", ConnectionID: "rtc", Level: 3, ExpiresAt: time.Now().Add(time.Minute)}
	invoke := func(method string, args any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(args)
		r := d.Handle(ctx, caller, wire.Request{Protocol: "hosts_tools/1", ID: wire.NewID("r_"), Action: "call", Call: &wire.Invocation{Domain: "exec", Command: "cua", Method: method, Args: raw}})
		if r.Error != nil {
			t.Fatalf("%s: %+v", method, r.Error)
		}
		raw, _ = json.Marshal(r.Result)
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return out
	}
	var window string
	for window == "" {
		r := invoke("window.list", map[string]any{"pid": cmd.Process.Pid})
		if rows, ok := r["data"].([]any); ok {
			for _, row := range rows {
				v := row.(map[string]any)
				if v["title"] == "AIC UI protocol fixture" {
					window = v["id"].(string)
				}
			}
		}
		if window != "" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("fixture window unavailable")
		case <-time.After(100 * time.Millisecond):
		}
	}
	invoke("window.observe", map[string]any{"window_id": window})
	invoke("window.fill", map[string]any{"window_id": window, "locator": map[string]any{"label": "Fixture name"}, "text": "typed native"})
	// The second transport addresses the same cua-owned window.
	caller.ConnectionID = "nats:fixture-owner"
	invoke("window.click", map[string]any{"window_id": window, "locator": map[string]any{"role": "button", "name": "Fixture save"}})
	for {
		var got struct {
			Value  string `json:"value"`
			Clicks int    `json:"clicks"`
		}
		b, _ := os.ReadFile(state)
		_ = json.Unmarshal(b, &got)
		if got.Value == "typed native" && got.Clicks == 1 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("fixture state: %s", b)
		case <-time.After(30 * time.Millisecond):
		}
	}
	r := invoke("window.observe", map[string]any{"window_id": window, "image": true})
	observation := r["observation"].(map[string]any)
	image := observation["image"].(map[string]any)
	total := 0
	for {
		part := invoke("observation.image.read", map[string]any{"window_id": window, "image_id": image["image_id"], "offset": total})
		var v ImagePart
		raw, _ := json.Marshal(part)
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		if v.Offset != total || len(v.Bytes) == 0 {
			t.Fatal("invalid image range")
		}
		total += len(v.Bytes)
		if v.EOF {
			break
		}
	}
	if total < 100 {
		t.Fatal("empty screenshot")
	}
}
