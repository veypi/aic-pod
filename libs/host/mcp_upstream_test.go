package host

import (
	"context"
	"encoding/json"
	"github.com/veypi/aic-pod/protocol"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/mcpx"
)

// This test launches the release's actual upstream servers. It is opt-in because
// agent-browser, Chrome and CuaDriver must be supplied by the bundle env vars.
func TestUpstreamMCPThroughExec(t *testing.T) {
	if os.Getenv("AIC_MCP_TEST") != "1" {
		t.Skip("set AIC_MCP_TEST=1 with upstream runtimes installed")
	}
	saved := cfg.Global
	cfg.Global = cfg.NewOptions()
	t.Cleanup(func() { cfg.Global = saved })
	work, err := os.MkdirTemp("", "aic-ab-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(work) })
	servers, err := mcpServers(nil, work)
	if err != nil {
		t.Fatal(err)
	}
	browser := servers["browser"]
	browser.Env["AGENT_BROWSER_SOCKET_DIR"] = filepath.Join(work, "runtime")
	browser.Env["AGENT_BROWSER_PROFILE"] = filepath.Join(work, "profile")
	servers["browser"] = browser
	c := New(Options{Key: "test.1.secret.owner", WorkDir: work, OnLog: t.Logf})
	t.Cleanup(func() { c.Close() })
	c.sessionRoot = filepath.Join(work, "sessions")
	if err := c.configureMCP(mcpx.Settings{Servers: servers}); err != nil {
		t.Fatal(err)
	}
	// This test uses isolated paths while exercising the built-in stream adapter.
	c.browserConfig = &browser
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	caller := testCaller()
	caller.Direct = true
	run := func(t *testing.T, script string, result any, input ...string) {
		t.Helper()
		request := execRequest(script, 30000)
		if len(input) > 0 {
			request.Exec.Stdin = input[0]
		}
		response := callTool(t, c, ctx, caller, request)
		if response.Error != nil {
			t.Fatal(response.Error)
		}
		out := decoded[protocol.Output](t, response.Result)
		if out.Attrs["exit_code"] != "0" {
			t.Fatalf("%s: %+v", script, out)
		}
		if err := json.Unmarshal([]byte(out.Content), result); err != nil {
			t.Fatalf("%s: %v (%s)", script, err, out.Content)
		}
	}
	for _, name := range []string{"browser", "cua"} {
		t.Run(name, func(t *testing.T) {
			session, err := c.mcpSession(ctx, name)
			if err != nil {
				t.Fatal(err)
			}
			var upstream []*mcp.Tool
			for tool, err := range session.Tools(ctx, nil) {
				if err != nil {
					t.Fatal(err)
				}
				upstream = append(upstream, tool)
			}
			var exposed []*mcp.Tool
			run(t, "mcp tools "+name, &exposed)
			want, _ := json.Marshal(upstream)
			got, _ := json.Marshal(exposed)
			if !reflect.DeepEqual(got, want) || len(exposed) == 0 {
				t.Fatalf("upstream catalog changed: %d tools", len(exposed))
			}
			t.Logf("%s: %d upstream tools unchanged", name, len(exposed))
		})
	}
	var permissions mcp.CallToolResult
	run(t, "mcp call cua check_permissions --json", &permissions)
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<title>Shared browser</title><button style="position:absolute;left:20px;top:20px;width:100px;height:60px" onclick="this.textContent=Number(this.textContent)+1">0</button><input style="position:absolute;left:20px;top:120px;width:200px;height:60px"><p id="clock"></p><script>setInterval(()=>document.querySelector('#clock').textContent=Date.now(),100)</script>`))
	}))
	defer fixture.Close()
	browserCall := func(name string, args any) mcp.CallToolResult {
		t.Helper()
		raw, _ := json.Marshal(args)
		var result mcp.CallToolResult
		run(t, "mcp call browser agent_browser_"+name+" --input - --json", &result, string(raw))
		if result.IsError {
			t.Fatalf("%s: %+v", name, result)
		}
		return result
	}
	data := func(result mcp.CallToolResult) map[string]any {
		t.Helper()
		return decoded[map[string]any](t, result.StructuredContent)["response"].(map[string]any)["data"].(map[string]any)
	}
	browserCall("tab_list", map[string]any{}) // Cold start must work for the UI's first call.
	browserCall("open", map[string]any{"url": fixture.URL})
	before, err := os.ReadFile(filepath.Join(work, "runtime", "aic.pid"))
	if err != nil {
		t.Fatal(err)
	}
	streamCtx, stopStream := context.WithCancel(ctx)
	input, output, done := make(chan []byte, 128), make(chan json.RawMessage, 128), make(chan error, 1)
	go func() {
		done <- c.streamBrowser(streamCtx, caller, input, func(raw []byte) error {
			select {
			case output <- append(json.RawMessage(nil), raw...):
				return nil
			case <-streamCtx.Done():
				return streamCtx.Err()
			}
		})
	}()
	defer stopStream()
	send := func(value any) { raw, _ := json.Marshal(value); input <- raw }
	frame := func() map[string]any {
		t.Helper()
		for {
			select {
			case raw := <-output:
				value := decoded[map[string]any](t, raw)
				if value["type"] == "frame" {
					return value
				}
			case err := <-done:
				t.Fatalf("stream closed: %v", err)
			case <-time.After(8 * time.Second):
				t.Fatal("frame timeout")
			}
		}
	}
	first := frame()
	if len(first["data"].(string)) == 0 {
		t.Fatal("empty frame")
	}
	// No ACK while the renderer is stalled: only one frame may be in flight.
	timer := time.NewTimer(250 * time.Millisecond)
waiting:
	for {
		select {
		case raw := <-output:
			if decoded[map[string]any](t, raw)["type"] == "frame" {
				t.Fatal("proxy acknowledged before renderer")
			}
		case <-timer.C:
			break waiting
		}
	}
	send(map[string]any{"type": "ack", "seq": first["seq"]})
	frame()
	for _, y := range []int{45, 145} {
		for _, event := range []string{"mousePressed", "mouseReleased"} {
			send(map[string]any{"type": "input_mouse", "eventType": event, "x": 60, "y": y, "button": "left", "clickCount": 1})
		}
	}
	for _, char := range "人工输入 abc" {
		send(map[string]any{"type": "input_keyboard", "eventType": "keyDown", "key": string(char), "text": string(char)})
		send(map[string]any{"type": "input_keyboard", "eventType": "keyUp", "key": string(char)})
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		state := data(browserCall("eval", map[string]any{"script": `({clicks:document.querySelector('button').textContent,text:document.querySelector('input').value})`}))
		result := state["result"].(map[string]any)
		if result["clicks"] == "1" && result["text"] == "人工输入 abc" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("manual input missing: %+v", state)
		}
		time.Sleep(50 * time.Millisecond)
	}
	browserCall("click", map[string]any{"selector": "button"})
	if result := data(browserCall("eval", map[string]any{"script": `document.querySelector('button').textContent`}))["result"]; result != "2" {
		t.Fatalf("MCP and UI diverged: %v", result)
	}
	browserCall("snapshot", map[string]any{})
	screenshot := browserCall("screenshot", map[string]any{"format": "jpeg"})
	hasImage := false
	for _, content := range screenshot.Content {
		if img, ok := content.(*mcp.ImageContent); ok && len(img.Data) > 0 {
			hasImage = true
		}
	}
	if !hasImage {
		t.Fatal("official screenshot image lost")
	}
	stopStream()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("viewer disconnect did not stop relay")
	}
	browserCall("snapshot", map[string]any{})
	after, err := os.ReadFile(filepath.Join(work, "runtime", "aic.pid"))
	if err != nil || strings.TrimSpace(string(before)) != strings.TrimSpace(string(after)) {
		t.Fatalf("browser daemon changed: %v", err)
	}
	t.Log("same daemon: manual click + Unicode input, MCP click/snapshot/screenshot, renderer ACK pacing, MCP after viewer disconnect")
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for {
		_, err := os.Stat(filepath.Join(work, "runtime", "aic.pid"))
		if os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon survived Pod shutdown: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
