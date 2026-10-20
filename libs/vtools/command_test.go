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
	r := NewRegistry(commands.NewRegistry(), Options{Gate: gate})
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
	r, _ := newFixture(t, func(context.Context, string, string) error {
		return fmt.Errorf("denied by exec rules; request grant cmd mcp.echo")
	})
	_, _, err := run(t, r, "echo", "ping")
	if exitCode(err) != 126 || !strings.Contains(err.Error(), "denied by exec rules") {
		t.Fatalf("gate: %v", err)
	}
}

// 命令路径的会话键来自注入的 Options.SessionOf（vtools 不反向依赖执行层）：
// 键取错会让会话级 grant cmd 在锁死的设备上静默失效。
func TestCommandGateSessionFromSessionOf(t *testing.T) {
	var got string
	r := NewRegistry(commands.NewRegistry(), Options{
		Gate:      func(_ context.Context, session, _ string) error { got = session; return nil },
		SessionOf: func(context.Context) string { return "s-42" },
	})
	if err := r.Register("echo", &fakeProvider{result: map[string]any{"ok": true}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run(t, r, "echo", "ping"); err != nil {
		t.Fatal(err)
	}
	if got != "s-42" {
		t.Fatalf("gate session = %q, want Options.SessionOf", got)
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

// --flag= 显式空串必须能传给 string 参数（解析层曾把 --flag= 与裸 flag
// 同映射为 raw==""，空串永远传不进去）；裸 --flag 仍报错。
func TestCommandEmptyStringFlag(t *testing.T) {
	r, p := newFixture(t, nil)
	if _, _, err := run(t, r, "echo", "echo", "--text="); err != nil {
		t.Fatal(err)
	}
	if got, ok := p.calls[0].args["text"]; !ok || got != "" {
		t.Fatalf("--text= 应传空串: %+v", p.calls[0].args)
	}
	if _, _, err := run(t, r, "echo", "echo", "--maybe="); err != nil {
		t.Fatal(err)
	}
	if got := p.calls[1].args["maybe"]; got != "" {
		t.Fatalf("--maybe= 应传空串: %+v", p.calls[1].args)
	}
	if _, _, err := run(t, r, "echo", "echo", "--text"); exitCode(err) != 2 || !strings.Contains(err.Error(), "requires a value") {
		t.Fatalf("裸 string flag 仍应报错: %v", err)
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

// --image-preview 支持性检查必须先于调用（不支持的 Provider 不应真跑工具）。
func TestCommandPreviewPrecheckBeforeCall(t *testing.T) {
	r, p := newFixture(t, nil)
	if _, _, err := run(t, r, "echo", "ping", "--image-preview", "1280x720"); exitCode(err) != 2 || !strings.Contains(err.Error(), "不支持 --image-preview") {
		t.Fatalf("preview gate: %v", err)
	}
	if len(p.calls) != 0 {
		t.Fatalf("unsupported preview still executed the tool: %+v", p.calls)
	}
}

// 命令面保留 flag（--json/--input/--image-preview/--help）遮蔽同名工具参数：
// 参数表给出提示，unknown-flag 的可映射清单不列保留名。
func TestCommandReservedFlagShadowed(t *testing.T) {
	r, _ := newFixture(t, nil)
	p2 := &fakeProvider{tools: []Tool{{Name: "cfg", InputSchema: json.RawMessage(`{
	  "type": "object",
	  "properties": {"json": {"type": "string"}, "text": {"type": "string"}}
	}`)}}}
	if err := r.Register("cfg", p2); err != nil {
		t.Fatal(err)
	}
	out, _, err := run(t, r, "cfg", "cfg", "--help")
	if err != nil || !strings.Contains(out, "保留 flag 名") {
		t.Fatalf("tool help must mark reserved names: %q %v", out, err)
	}
	_, _, err = run(t, r, "cfg", "cfg", "--nope", "1")
	if exitCode(err) != 2 || !strings.Contains(err.Error(), "--text") || strings.Contains(err.Error(), "--json ") {
		t.Fatalf("mappable list must exclude reserved names: %v", err)
	}
	// 保留名参数走 --input 照常到达
	if _, _, err := run(t, r, "cfg", "cfg", "--input", `{"json":"v"}`); err != nil {
		t.Fatal(err)
	}
	if got := p2.calls[0].args["json"]; got != "v" {
		t.Fatalf("reserved-named arg via --input: %+v", p2.calls[0].args)
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

type streamProvider struct {
	fakeProvider
	channel string
}

func (s *streamProvider) StreamChannel() string { return s.channel }

func (s *streamProvider) Stream(context.Context, protocol.Caller, <-chan []byte, func([]byte) error) error {
	return nil
}

type fakeStreamer struct {
	channel string
}

func (s *fakeStreamer) StreamChannel() string { return s.channel }

func (s *fakeStreamer) Stream(context.Context, protocol.Caller, <-chan []byte, func([]byte) error) error {
	return nil
}

// RegisterCustom 的 cmd.Name() 必须等于 alias（vsh 以 Name 为键、Unregister
// 按 alias 摘，不一致会永久驻留）。
func TestRegistryCustomNameMismatch(t *testing.T) {
	r, _ := newFixture(t, nil)
	cmd := commands.DefineCommand("other-name", func(context.Context, *commands.Invocation) error { return nil })
	if err := r.RegisterCustom("svc", &fakeProvider{}, cmd); err == nil {
		t.Fatal("name mismatch accepted")
	}
	if r.Has("svc") {
		t.Fatal("rejected RegisterCustom left an entry")
	}
}

// stream channel 标签撞名即拒（登记侧防护；Unregister 只摘自己的映射）。
func TestRegistryStreamChannelCollision(t *testing.T) {
	r, _ := newFixture(t, nil)
	if err := r.RegisterStream("echo", &fakeStreamer{channel: "aic-echo"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register("other", &fakeProvider{}); err != nil {
		t.Fatal(err)
	}
	// RegisterStream 抢占他人标签即拒
	if err := r.RegisterStream("other", &fakeStreamer{channel: "aic-echo"}); err == nil {
		t.Fatal("channel takeover accepted")
	}
	// Provider 自带 Streamer 撞标签也拒
	if err := r.Register("third", &streamProvider{channel: "aic-echo"}); err == nil {
		t.Fatal("register with taken channel accepted")
	}
	if r.Has("third") {
		t.Fatal("rejected registration left an entry")
	}
	// 标签仍属 echo
	if _, ok := r.StreamHandler("aic-echo"); !ok {
		t.Fatal("original channel mapping lost")
	}
	r.Unregister("echo")
	if _, ok := r.StreamHandler("aic-echo"); ok {
		t.Fatal("channel survived owner Unregister")
	}
	// 摘除后标签可重用
	if err := r.RegisterStream("other", &fakeStreamer{channel: "aic-echo"}); err != nil {
		t.Fatalf("channel reuse after unregister: %v", err)
	}
}

func TestRegistryStreamHandler(t *testing.T) {
	gated := false
	var gateSession string
	r, _ := newFixture(t, func(_ context.Context, session, _ string) error { gated = true; gateSession = session; return nil })
	s := &fakeStreamer{channel: "aic-echo"}
	if err := r.RegisterStream("echo", s); err != nil {
		t.Fatal(err)
	}
	relay, ok := r.StreamHandler("aic-echo")
	if !ok {
		t.Fatal("stream channel not registered")
	}
	if err := relay(context.Background(), protocol.Caller{Origin: "s1"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !gated {
		t.Fatal("stream open bypassed the gate")
	}
	// 流通道开门的会话键 = caller.Origin（peer ctx 不携带会话；用错键会让
	// 会话级 grant cmd 在锁死设备上静默失效）
	if gateSession != "s1" {
		t.Fatalf("stream gate session = %q, want caller.Origin", gateSession)
	}
	if _, ok := r.StreamHandler("aic-other"); ok {
		t.Fatal("unknown channel resolved")
	}
	r.Unregister("echo")
	if _, ok := r.StreamHandler("aic-echo"); ok {
		t.Fatal("stream channel survived Unregister")
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
