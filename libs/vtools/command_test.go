package vtools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

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

func newFixture(t *testing.T) *fakeProvider {
	t.Helper()
	return &fakeProvider{
		tools: []Tool{
			{Name: "echo", Description: "回显", InputSchema: echoSchema},
			{Name: "ping", Description: ""},
		},
		result: map[string]any{"ok": true},
	}
}

// run 直接走调用内核；注册/撞名由 command hub 的测试覆盖。
func run(t *testing.T, p Provider, alias string, args ...string) (string, string, error) {
	t.Helper()
	var out, errBuf strings.Builder
	err := RunProvider(context.Background(), &commands.Invocation{Args: args, Stdout: &out, Stderr: &errBuf}, alias, p)
	return out.String(), errBuf.String(), err
}

func exitCode(err error) int {
	code, _ := commands.ExitCode(err)
	return code
}

func TestCommandCatalog(t *testing.T) {
	p := newFixture(t)
	out, _, err := run(t, p, "echo")
	if err != nil || !strings.Contains(out, "echo\t回显") || !strings.Contains(out, "ping") {
		t.Fatalf("catalog: %q %v", out, err)
	}
	out, _, err = run(t, p, "echo", "--json")
	if err != nil || !strings.Contains(out, `"inputSchema"`) {
		t.Fatalf("catalog --json: %q %v", out, err)
	}
}

func TestCommandFlagMapping(t *testing.T) {
	p := newFixture(t)
	if _, _, err := run(t, p, "echo", "echo", "--text", "hi", "--times", "3", "--ratio", "0.5", "--loud"); err != nil {
		t.Fatal(err)
	}
	args := p.calls[0].args
	if args["text"] != "hi" || args["times"] != int64(3) || args["ratio"] != 0.5 || args["loud"] != true {
		t.Fatalf("mapped args: %+v", args)
	}
	// --flag=value 与可空类型
	if _, _, err := run(t, p, "echo", "echo", "--maybe=yo"); err != nil {
		t.Fatal(err)
	}
	if got := p.calls[1].args["maybe"]; got != "yo" {
		t.Fatalf("nullable string: %+v", got)
	}
}

// --flag= 显式空串必须能传给 string 参数（解析层曾把 --flag= 与裸 flag
// 同映射为 raw==""，空串永远传不进去）；裸 --flag 仍报错。
func TestCommandEmptyStringFlag(t *testing.T) {
	p := newFixture(t)
	if _, _, err := run(t, p, "echo", "echo", "--text="); err != nil {
		t.Fatal(err)
	}
	if got, ok := p.calls[0].args["text"]; !ok || got != "" {
		t.Fatalf("--text= 应传空串: %+v", p.calls[0].args)
	}
	if _, _, err := run(t, p, "echo", "echo", "--maybe="); err != nil {
		t.Fatal(err)
	}
	if got := p.calls[1].args["maybe"]; got != "" {
		t.Fatalf("--maybe= 应传空串: %+v", p.calls[1].args)
	}
	if _, _, err := run(t, p, "echo", "echo", "--text"); exitCode(err) != 2 || !strings.Contains(err.Error(), "requires a value") {
		t.Fatalf("裸 string flag 仍应报错: %v", err)
	}
}

func TestCommandInputMergeAndFlagOverride(t *testing.T) {
	p := newFixture(t)
	if _, _, err := run(t, p, "echo", "echo", "--input", `{"text":"a","meta":{"k":1}}`, "--text", "b"); err != nil {
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
	p := newFixture(t)
	if _, _, err := run(t, p, "echo", "echo", "--nope", "1"); exitCode(err) != 2 || !strings.Contains(err.Error(), "unknown flag --nope") {
		t.Fatalf("unknown flag: %v", err)
	}
	if _, _, err := run(t, p, "echo", "echo", "--times", "soon"); exitCode(err) != 2 || !strings.Contains(err.Error(), "invalid integer") {
		t.Fatalf("integer: %v", err)
	}
	if _, _, err := run(t, p, "echo", "echo", "--meta", "{}"); exitCode(err) != 2 || !strings.Contains(err.Error(), "cannot be mapped to a flag") {
		t.Fatalf("non-scalar must point at --input: %v", err)
	}
	if _, _, err := run(t, p, "echo", "echo", "--text"); exitCode(err) != 2 || !strings.Contains(err.Error(), "requires a value") {
		t.Fatalf("bare string flag: %v", err)
	}
	if _, _, err := run(t, p, "echo", "echo", "--input", `[1]`); exitCode(err) != 2 {
		t.Fatalf("non-object input: %v", err)
	}
}

func TestCommandUnknownToolListsAvailable(t *testing.T) {
	p := newFixture(t)
	// 无 flag 时直达（不校验工具名，由服务方报错）；有 flag 时先校验并给可用面
	if _, _, err := run(t, p, "echo", "ehco", "--text", "x"); exitCode(err) != 1 || !strings.Contains(err.Error(), "available: echo, ping") {
		t.Fatalf("unknown tool: %v", err)
	}
}

func TestCommandToolHelp(t *testing.T) {
	p := newFixture(t)
	out, _, err := run(t, p, "echo", "echo", "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--text (string) required", "--times (integer) default 1", "echo 文本"} {
		if !strings.Contains(out, want) {
			t.Fatalf("tool help missing %q:\n%s", want, out)
		}
	}
}

func TestCommandCallErrorRendersResult(t *testing.T) {
	p := newFixture(t)
	p.result = map[string]any{"isError": true, "content": "boom"}
	p.callErr = &CallError{Result: p.result}
	out, _, err := run(t, p, "echo", "ping", "--json")
	if exitCode(err) != 1 || !strings.Contains(out, `"boom"`) {
		t.Fatalf("CallError must render result with exit 1: %q %v", out, err)
	}
}

// --image-preview 支持性检查必须先于调用（不支持的 Provider 不应真跑工具）。
func TestCommandPreviewPrecheckBeforeCall(t *testing.T) {
	p := newFixture(t)
	if _, _, err := run(t, p, "echo", "ping", "--image-preview", "1280x720"); exitCode(err) != 2 || !strings.Contains(err.Error(), "does not support --image-preview") {
		t.Fatalf("preview gate: %v", err)
	}
	if len(p.calls) != 0 {
		t.Fatalf("unsupported preview still executed the tool: %+v", p.calls)
	}
}

// 命令面保留 flag（--json/--input/--image-preview/--help）遮蔽同名工具参数：
// 参数表给出提示，unknown-flag 的可映射清单不列保留名。
func TestCommandReservedFlagShadowed(t *testing.T) {
	p2 := &fakeProvider{tools: []Tool{{Name: "cfg", InputSchema: json.RawMessage(`{
	  "type": "object",
	  "properties": {"json": {"type": "string"}, "text": {"type": "string"}}
	}`)}}}
	out, _, err := run(t, p2, "cfg", "cfg", "--help")
	if err != nil || !strings.Contains(out, "reserved flag name") {
		t.Fatalf("tool help must mark reserved names: %q %v", out, err)
	}
	_, _, err = run(t, p2, "cfg", "cfg", "--nope", "1")
	if exitCode(err) != 2 || !strings.Contains(err.Error(), "--text") || strings.Contains(err.Error(), "--json ") {
		t.Fatalf("mappable list must exclude reserved names: %v", err)
	}
	// 保留名参数走 --input 照常到达
	if _, _, err := run(t, p2, "cfg", "cfg", "--input", `{"json":"v"}`); err != nil {
		t.Fatal(err)
	}
	if got := p2.calls[0].args["json"]; got != "v" {
		t.Fatalf("reserved-named arg via --input: %+v", p2.calls[0].args)
	}
}

// 重复 --image-preview 是参数面错误（exit 2），不是静默 last-wins——且必须
// 先于任何真实调用（不支持的副作用不能先落地）。
func TestDuplicateImagePreviewRejectedBeforeCall(t *testing.T) {
	p := &fakeProvider{result: map[string]any{"ok": true}}
	var out, errBuf strings.Builder
	err := RunProvider(context.Background(), &commands.Invocation{
		Args:   []string{"cap", "--image-preview", "64x64", "--image-preview", "32x32"},
		Stdout: &out, Stderr: &errBuf,
	}, "mcp", p)
	if exitCode(err) != 2 {
		t.Fatalf("duplicate --image-preview: %v", err)
	}
	if len(p.calls) != 0 {
		t.Fatal("rejected flag parse must not reach the provider")
	}
}
