package vtools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/veypi/aic-pod/protocol"
	"github.com/veypi/vsh/commands"
)

type recordedCall struct {
	tool string
	args map[string]any
}

type fakeProvider struct {
	tools   []Tool
	result  any
	callErr error
	calls   []recordedCall
}

func (f *fakeProvider) Tools(context.Context) ([]Tool, error) { return f.tools, nil }

func (f *fakeProvider) Call(_ context.Context, tool string, args map[string]any) (any, error) {
	f.calls = append(f.calls, recordedCall{tool: tool, args: args})
	if f.callErr != nil {
		return f.result, f.callErr
	}
	return f.result, nil
}

var echoSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "text":   {"type": "string", "description": "echo 文本"},
    "times":  {"type": "integer", "default": 1},
    "ratio":  {"type": "number"},
    "loud":   {"type": "boolean"},
    "meta":   {"type": "object", "description": "复杂参数"},
    "maybe":  {"type": ["string", "null"]}
  },
  "required": ["text"]
}`)

func newFixture(t *testing.T, gate Gate) (*Registry, *fakeProvider) {
	t.Helper()
	p := &fakeProvider{
		tools: []Tool{
			{Name: "echo", Description: "回显", InputSchema: echoSchema},
			{Name: "ping", Description: ""},
		},
		result: map[string]any{"ok": true},
	}
	r := NewRegistry(commands.NewRegistry(), gate, nil)
	if err := r.Register("echo", p); err != nil {
		t.Fatal(err)
	}
	return r, p
}

func run(t *testing.T, r *Registry, alias string, args ...string) (string, string, error) {
	t.Helper()
	cmd, ok := r.cmds.Lookup(alias)
	if !ok {
		t.Fatalf("command %q not registered", alias)
	}
	var out, errBuf strings.Builder
	err := cmd.Run(context.Background(), &commands.Invocation{Args: args, Stdout: &out, Stderr: &errBuf})
	return out.String(), errBuf.String(), err
}

func exitCode(err error) int {
	code, _ := commands.ExitCode(err)
	return code
}

func TestCommandCatalog(t *testing.T) {
	r, _ := newFixture(t, nil)
	out, _, err := run(t, r, "echo")
	if err != nil || !strings.Contains(out, "echo\t回显") || !strings.Contains(out, "ping") {
		t.Fatalf("catalog: %q %v", out, err)
	}
	out, _, err = run(t, r, "echo", "--json")
	if err != nil || !strings.Contains(out, `"inputSchema"`) {
		t.Fatalf("catalog --json: %q %v", out, err)
	}
}

func TestCommandGateDeny(t *testing.T) {
	r, _ := newFixture(t, func(context.Context, string) error {
		return fmt.Errorf("denied by exec rules; request grant cmd mcp.echo")
	})
	_, _, err := run(t, r, "echo", "ping")
	if exitCode(err) != 126 || !strings.Contains(err.Error(), "denied by exec rules") {
		t.Fatalf("gate: %v", err)
	}
}

func TestCommandFlagMapping(t *testing.T) {
	r, p := newFixture(t, nil)
	if _, _, err := run(t, r, "echo", "echo", "--text", "hi", "--times", "3", "--ratio", "0.5", "--loud"); err != nil {
		t.Fatal(err)
	}
	args := p.calls[0].args
	if args["text"] != "hi" || args["times"] != int64(3) || args["ratio"] != 0.5 || args["loud"] != true {
		t.Fatalf("mapped args: %+v", args)
	}
	// --flag=value 与可空类型
	if _, _, err := run(t, r, "echo", "echo", "--maybe=yo"); err != nil {
		t.Fatal(err)
	}
	if got := p.calls[1].args["maybe"]; got != "yo" {
		t.Fatalf("nullable string: %+v", got)
	}
}

func TestCommandInputMergeAndFlagOverride(t *testing.T) {
	r, p := newFixture(t, nil)
	if _, _, err := run(t, r, "echo", "echo", "--input", `{"text":"a","meta":{"k":1}}`, "--text", "b"); err != nil {
		t.Fatal(err)
	}
	args := p.calls[0].args
	if args["text"] != "b" {
		t.Fatalf("flag must override --input key: %+v", args)
	}
	if meta, ok := args["meta"].(map[string]any); !ok || meta["k"] != float64(1) {
		t.Fatalf("complex key from --input lost: %+v", args)
	}
}

func TestCommandFlagErrors(t *testing.T) {
	r, _ := newFixture(t, nil)
	if _, _, err := run(t, r, "echo", "echo", "--nope", "1"); exitCode(err) != 2 || !strings.Contains(err.Error(), "unknown flag --nope") {
		t.Fatalf("unknown flag: %v", err)
	}
	if _, _, err := run(t, r, "echo", "echo", "--times", "soon"); exitCode(err) != 2 || !strings.Contains(err.Error(), "invalid integer") {
		t.Fatalf("integer: %v", err)
	}
	if _, _, err := run(t, r, "echo", "echo", "--meta", "{}"); exitCode(err) != 2 || !strings.Contains(err.Error(), "不能映射为 flag") {
		t.Fatalf("non-scalar must point at --input: %v", err)
	}
	if _, _, err := run(t, r, "echo", "echo", "--text"); exitCode(err) != 2 || !strings.Contains(err.Error(), "requires a value") {
		t.Fatalf("bare string flag: %v", err)
	}
	if _, _, err := run(t, r, "echo", "echo", "--input", `[1]`); exitCode(err) != 2 {
		t.Fatalf("non-object input: %v", err)
	}
}

func TestCommandUnknownToolListsAvailable(t *testing.T) {
	r, _ := newFixture(t, nil)
	// 无 flag 时直达（不校验工具名，由服务方报错）；有 flag 时先校验并给可用面
	if _, _, err := run(t, r, "echo", "ehco", "--text", "x"); exitCode(err) != 1 || !strings.Contains(err.Error(), "可用: echo, ping") {
		t.Fatalf("unknown tool: %v", err)
	}
}

func TestCommandToolHelp(t *testing.T) {
	r, _ := newFixture(t, nil)
	out, _, err := run(t, r, "echo", "echo", "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--text (string) 必填", "--times (integer) 默认 1", "echo 文本"} {
		if !strings.Contains(out, want) {
			t.Fatalf("tool help missing %q:\n%s", want, out)
		}
	}
}

func TestCommandCallErrorRendersResult(t *testing.T) {
	r, p := newFixture(t, nil)
	p.result = map[string]any{"isError": true, "content": "boom"}
	p.callErr = &CallError{Result: p.result}
	out, _, err := run(t, r, "echo", "ping", "--json")
	if exitCode(err) != 1 || !strings.Contains(out, `"boom"`) {
		t.Fatalf("CallError must render result with exit 1: %q %v", out, err)
	}
}

func TestCommandPreviewUnsupported(t *testing.T) {
	r, _ := newFixture(t, nil)
	if _, _, err := run(t, r, "echo", "ping", "--image-preview", "1280x720"); exitCode(err) != 2 || !strings.Contains(err.Error(), "不支持 --image-preview") {
		t.Fatalf("preview gate: %v", err)
	}
}

// ---- Registry 生命周期 ----

func TestRegistryLifecycle(t *testing.T) {
	r, _ := newFixture(t, nil)
	if !r.Has("echo") {
		t.Fatal("registered service missing")
	}
	if err := r.Register("echo", &fakeProvider{}); err == nil {
		t.Fatal("duplicate Register accepted")
	}
	if err := r.Register("bad name!", &fakeProvider{}); err == nil {
		t.Fatal("invalid alias accepted")
	}
	// 撞内建名即拒
	if err := r.cmds.RegisterGuarded(commands.DefineCommand("taken", nil)); err != nil {
		t.Fatal(err)
	}
	if err := r.Register("taken", &fakeProvider{}); err == nil {
		t.Fatal("name collision with built-in accepted")
	}
	r.Unregister("echo")
	if r.Has("echo") {
		t.Fatal("unregistered service still present")
	}
	if _, ok := r.cmds.Lookup("echo"); ok {
		t.Fatal("vsh command still resolves after Unregister")
	}
	r.Unregister("echo") // 幂等
	aliases := r.Aliases()
	if len(aliases) != 0 {
		t.Fatalf("aliases: %v", aliases)
	}
}

type fakeStreamer struct {
	channel string
}

func (s *fakeStreamer) StreamChannel() string { return s.channel }

func (s *fakeStreamer) Stream(context.Context, protocol.Caller, <-chan []byte, func([]byte) error) error {
	return nil
}

func TestRegistryStreamHandler(t *testing.T) {
	gated := false
	r, _ := newFixture(t, func(context.Context, string) error { gated = true; return nil })
	s := &fakeStreamer{channel: "aic-echo"}
	if err := r.RegisterStream("echo", s); err != nil {
		t.Fatal(err)
	}
	relay, ok := r.StreamHandler("aic-echo")
	if !ok {
		t.Fatal("stream channel not registered")
	}
	if err := relay(context.Background(), protocol.Caller{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !gated {
		t.Fatal("stream open bypassed the gate")
	}
	if _, ok := r.StreamHandler("aic-other"); ok {
		t.Fatal("unknown channel resolved")
	}
	r.Unregister("echo")
	if _, ok := r.StreamHandler("aic-echo"); ok {
		t.Fatal("stream channel survived Unregister")
	}
}
