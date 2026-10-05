package mcpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/veypi/vsh/commands"
)

func TestCommandPreservesResultsAndExitStatus(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	backend := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	calls := 0
	backend.AddTool(&mcp.Tool{Name: "save", InputSchema: map[string]any{"type": "object"}}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls++
		var args map[string]any
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		return &mcp.CallToolResult{IsError: true, StructuredContent: args, Content: []mcp.Content{
			&mcp.TextContent{Text: "partial result"},
			&mcp.ImageContent{Data: []byte{1, 2, 3}, MIMEType: "image/png"},
			&mcp.ResourceLink{URI: "result://saved", Name: "saved"},
		}}, nil
	})
	backend.AddResource(&mcp.Resource{URI: "result://saved", Name: "saved"}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: "result://saved", Text: "persistent resource"}}}, nil
	})
	connect := func(server *mcp.Server) *mcp.ClientSession {
		st, ct := mcp.NewInMemoryTransports()
		ss, err := server.Connect(ctx, st, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ss.Close() })
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cs.Close() })
		return cs
	}
	upstream := connect(backend)
	command := Command(func(_ context.Context, server string) (*mcp.ClientSession, error) {
		if server != "fixture" {
			t.Fatalf("wrong server %q", server)
		}
		return upstream, nil
	})
	var stdout, stderr bytes.Buffer
	// Discovery and describe expose the upstream schema verbatim, including
	// names/annotations. The command must not register a second tool catalog.
	var original []*mcp.Tool
	for tool, err := range upstream.Tools(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		original = append(original, tool)
	}
	if err := command(ctx, &commands.Invocation{Args: []string{"tools", "fixture"}, Stdout: &stdout, Stderr: &stderr}); err != nil {
		t.Fatal(err)
	}
	var listed []any
	if err := json.Unmarshal(stdout.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(original)
	var expected []any
	if err := json.Unmarshal(raw, &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(listed, expected) {
		t.Fatalf("tool catalog changed: %s", stdout.String())
	}
	stdout.Reset()
	if err := command(ctx, &commands.Invocation{Args: []string{"describe", "fixture", "save"}, Stdout: &stdout, Stderr: &stderr}); err != nil {
		t.Fatal(err)
	}
	var described any
	if err := json.Unmarshal(stdout.Bytes(), &described); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(described, expected[0]) {
		t.Fatalf("tool schema changed: %s", stdout.String())
	}
	stdout.Reset()
	inv := &commands.Invocation{Args: []string{"call", "fixture", "save", "--input", "-", "--json"}, Stdin: strings.NewReader(`{"title":"$(touch /bad) ; quote ' ","count":3}`), Stdout: &stdout, Stderr: &stderr}
	err := command(ctx, inv)
	var exit *commands.ExitError
	if !errors.As(err, &exit) || exit.Code != 1 {
		t.Fatalf("expected failed tool exit, got %v", err)
	}
	var result mcp.CallToolResult
	if err = json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	args := result.StructuredContent.(map[string]any)
	if !result.IsError || len(result.Content) != 3 || args["title"] != "$(touch /bad) ; quote ' " || calls != 1 {
		t.Fatalf("result altered: %s calls=%d", stdout.String(), calls)
	}
	if image, ok := result.Content[1].(*mcp.ImageContent); !ok || !bytes.Equal(image.Data, []byte{1, 2, 3}) {
		t.Fatalf("image altered: %+v", result.Content)
	}
	stdout.Reset()
	inv.Args = []string{"read", "fixture", "result://saved", "--json"}
	if err = command(ctx, inv); err != nil {
		t.Fatal(err)
	}
	var resource mcp.ReadResourceResult
	if err = json.Unmarshal(stdout.Bytes(), &resource); err != nil {
		t.Fatal(err)
	}
	if len(resource.Contents) != 1 || resource.Contents[0].Text != "persistent resource" {
		t.Fatal(stdout.String())
	}
}

// 下游提前关闭（`mcp tools x | head`）必须按 SIGPIPE 语义收尾：返回
// ExitError{141}，而不是裸 error（裸 error 会被解释器当作致命中止，
// 把常用的截断管道变成整段脚本中断）。
func TestCommandTreatsClosedPipeAsSigpipe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	backend := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	backend.AddTool(&mcp.Tool{Name: "save", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	})
	st, ct := mcp.NewInMemoryTransports()
	ss, err := backend.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	command := Command(func(context.Context, string) (*mcp.ClientSession, error) { return cs, nil })
	inv := &commands.Invocation{Args: []string{"tools", "fixture"}, Stdout: closedPipeWriter{}, Stderr: io.Discard}
	err = command(ctx, inv)
	var exit *commands.ExitError
	if !errors.As(err, &exit) || exit.Code != 141 {
		t.Fatalf("closed pipe must exit 141, got %v", err)
	}
}

type closedPipeWriter struct{}

func (closedPipeWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestCommandRejectsTargetBeforeResolvingService(t *testing.T) {
	command := Command(func(context.Context, string) (*mcp.ClientSession, error) {
		t.Fatal("resolver called for invalid command")
		return nil, nil
	})
	var out bytes.Buffer
	err := command(context.Background(), &commands.Invocation{Args: []string{"--target", "other-host", "tools", "fixture"}, Stdout: &out, Stderr: &out})
	var exit *commands.ExitError
	if !errors.As(err, &exit) || exit.Code != 2 || !strings.Contains(out.String(), "unknown option --target") {
		t.Fatalf("%v %s", err, &out)
	}
}
