//go:build darwin

package cua

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
	"github.com/veypi/vsh/commands"
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	caller := wire.Caller{Subject: "fixture-owner", ConnectionID: "rtc", ExpiresAt: time.Now().Add(time.Minute)}
	vshCmd := s.VshCommand(func(context.Context) wire.Caller { return caller })
	invoke := func(args ...string) map[string]any {
		t.Helper()
		var out, errBuf bytes.Buffer
		inv := &commands.Invocation{Args: append(args, "--json"), Env: map[string]string{}, Stdout: &out, Stderr: &errBuf}
		if err := commands.RunCommand(ctx, vshCmd, inv); err != nil {
			t.Fatalf("%v: %v (%s)", args, err, errBuf.String())
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &m); err != nil {
			t.Fatalf("%v: stdout not JSON: %q", args, out.String())
		}
		return m
	}
	var window string
	for window == "" {
		r := invoke("window.list", "--pid", strconv.Itoa(cmd.Process.Pid))
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
	invoke("window.observe", window)
	invoke("window.fill", window, "--label", "Fixture name", "--text", "typed native")
	invoke("window.click", window, "--role", "button", "--name", "Fixture save")
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
	r := invoke("window.observe", window, "--image")
	observation := r["observation"].(map[string]any)
	image := observation["image"].(map[string]any)
	total := 0
	for {
		part := invoke("observation.image.read", window, image["image_id"].(string), "--offset", strconv.Itoa(total))
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
