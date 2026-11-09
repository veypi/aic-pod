package host

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/protocol"
)

func callTool(t *testing.T, c *Client, ctx context.Context, caller protocol.Caller, r protocol.Request) protocol.Response {
	t.Helper()
	r.Protocol = protocol.RtcProtocol
	return c.HandleTool(ctx, caller, r)
}

func callNATS(t *testing.T, c *Client, subject string, raw []byte) protocol.Response {
	t.Helper()
	return c.HandleNATS(context.Background(), subject, raw)
}

func TestMCPCommandIsRemoved(t *testing.T) {
	withGlobal(t, func(o *cfg.Options) { o.ExecPolicy = cfg.PolicyOpen })
	c, _ := testClient(t)
	response := callTool(t, c, context.Background(), testCaller(), execRequest("mcp tools aic", 30000))
	out := decoded[protocol.Output](t, response.Result)
	if response.Error != nil || out.Attrs["exit_code"] != "127" {
		t.Fatalf("removed mcp command still resolves: %+v %+v", response, out)
	}
	// The normal platform path still works with no configured MCP services.
	if result := callTool(t, c, context.Background(), testCaller(), fsRequest("roots", map[string]any{})); result.Error != nil {
		t.Fatal(result.Error)
	}
}

func TestUnifiedMCPCommandFromHTTPAndCancellation(t *testing.T) {
	withGlobal(t, func(o *cfg.Options) { o.ExecPolicy, o.NetPolicy = cfg.PolicyOpen, cfg.PolicyOpen })
	var calls atomic.Int32
	service := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	mcp.AddTool(service, &mcp.Tool{Name: "next", Description: "Increment service state"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return nil, map[string]any{"count": calls.Add(1)}, nil
	})
	started, cancelled := make(chan struct{}), make(chan struct{})
	mcp.AddTool(service, &mcp.Tool{Name: "wait", Description: "Wait until cancelled"}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		close(started)
		select {
		case <-ctx.Done():
			close(cancelled)
			return nil, nil, ctx.Err()
		case <-time.After(10 * time.Second):
			return nil, nil, context.DeadlineExceeded
		}
	})
	endpoint := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return service }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, PropagateRequestCancellation: true}))
	defer endpoint.Close()
	c, _ := testClient(t)
	if _, err := runHostCommandManagement(t, c, "add", "fixture", "--protocol", "mcp", "--url", endpoint.URL); err != nil {
		t.Fatal(err)
	}
	for _, want := range []int{1, 2, 3} {
		caller := testCaller()
		caller.Direct = want == 3 // UI uses the same exec path with a direct RTC caller.
		response := callTool(t, c, context.Background(), caller, execRequest("fixture next --json", 30000))
		output := decoded[protocol.Output](t, response.Result)
		var result mcp.CallToolResult
		if response.Error != nil || output.Attrs["exit_code"] != "0" || json.Unmarshal([]byte(output.Content), &result) != nil {
			t.Fatalf("native exec service call: %+v %+v", response, output)
		}
		var value struct {
			Count int `json:"count"`
		}
		raw, err := json.Marshal(result.StructuredContent)
		if err == nil {
			err = json.Unmarshal(raw, &value)
		}
		if err != nil || value.Count != want {
			t.Fatalf("state lost: %+v %v", value, err)
		}
	}
	// The UI's native cancel reaches the SDK call without closing the service.
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	caller := testCaller()
	caller.Direct = true
	request := execRequest("fixture wait --json", 30000)
	finished := make(chan protocol.Response, 1)
	go func() { finished <- callTool(t, c, ctx, caller, request) }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("command did not reach MCP service")
	}
	response := callTool(t, c, ctx, caller, protocol.Request{ID: "cancel_mcp", Action: protocol.ActionCancel, CancelID: request.ID})
	if response.Error != nil {
		t.Fatal(response.Error)
	}
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal("native cancellation did not stop command")
	}
	select {
	case <-cancelled:
	case <-ctx.Done():
		t.Fatal("native cancellation did not reach MCP service")
	}
	response = callTool(t, c, ctx, caller, execRequest("fixture next --json", 30000))
	out := decoded[protocol.Output](t, response.Result)
	if response.Error != nil || out.Attrs["exit_code"] != "0" || calls.Load() != 4 {
		t.Fatalf("service unusable after cancellation: %+v %+v", response, out)
	}
}

func TestMCPCommandIsNotDiscoverable(t *testing.T) {
	c, _ := testClient(t)
	response := callTool(t, c, context.Background(), testCaller(), execRequest("commands", 30000))
	out := decoded[protocol.Output](t, response.Result)
	for _, line := range strings.Split(out.Content, "\n") {
		if strings.TrimSpace(line) == "mcp" {
			t.Fatal("removed mcp command is still discoverable")
		}
	}
}

func TestMCPImageUsesExecResultAttrs(t *testing.T) {
	withGlobal(t, func(o *cfg.Options) { o.ExecPolicy, o.NetPolicy = cfg.PolicyOpen, cfg.PolicyOpen })
	var raw bytes.Buffer
	if err := png.Encode(&raw, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	service := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	mcp.AddTool(service, &mcp.Tool{Name: "shot"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			StructuredContent: map[string]any{"capture_id": "one"},
			Content:           []mcp.Content{&mcp.ImageContent{Data: raw.Bytes(), MIMEType: "image/png"}},
		}, nil, nil
	})
	endpoint := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return service }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
	defer endpoint.Close()
	c, _ := testClient(t)
	if _, err := runHostCommandManagement(t, c, "add", "fixture", "--protocol", "mcp", "--url", endpoint.URL); err != nil {
		t.Fatal(err)
	}
	for _, direct := range []bool{false, true} {
		caller := testCaller()
		caller.Direct = direct
		response := callTool(t, c, context.Background(), caller, execRequest("fixture shot --json", 30000))
		out := decoded[protocol.Output](t, response.Result)
		if response.Error != nil || !strings.HasPrefix(out.Attrs["image_data"], "data:image/png;base64,") {
			t.Fatalf("direct=%t response=%+v attrs=%v", direct, response, out.Attrs)
		}
		if strings.Contains(out.Content, base64.StdEncoding.EncodeToString(raw.Bytes())) {
			t.Fatalf("direct=%t stdout still contains image base64", direct)
		}
		var result mcp.CallToolResult
		if err := json.Unmarshal([]byte(out.Content), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Content[0].(*mcp.ImageContent).Data) != 0 {
			t.Fatalf("direct=%t image data was not detached", direct)
		}
	}
}
