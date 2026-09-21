package host

import (
	"context"
	"encoding/json"
	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/exec_procs"
	tool "github.com/veypi/aic-pod/libs/hosts_tool"
	"github.com/veypi/aic-pod/libs/vcore"
	fsp "github.com/veypi/aic-pod/protocol/fs"
	natswire "github.com/veypi/aic-pod/protocol/hosts_nats"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T) (*Client, string) {
	t.Helper()
	c := New(Options{Key: "host_1.1.secret.owner", WorkDir: t.TempDir(), BrowserStateDir: t.TempDir(), NoSandbox: true})
	c.hostID = "host_1"
	c.uid = "owner"
	c.kTool = "test-key"
	c.sessionRoot = t.TempDir()
	if c.initErr != nil {
		t.Fatal(c.initErr)
	}
	t.Cleanup(func() { c.Close() })
	return c, c.kTool
}
func signedCall(t *testing.T, c *Client, req wire.Request, level int, origin, scope string) wire.Response {
	t.Helper()
	req.Protocol = natswire.Protocol
	if req.ID == "" {
		req.ID = wire.NewID("r_")
	}
	route, _ := natswire.Subject(c.uid, c.hostID)
	r := natswire.Request{HostID: c.hostID, Subject: route, Caller: c.uid, Origin: origin, Scope: scope, GrantedLevel: level, Nonce: wire.NewID("n_"), Deadline: time.Now().Add(time.Minute).UnixMilli(), AuthorizationUntil: time.Now().Add(2 * time.Minute).UnixMilli(), Request: req}
	natswire.Sign(c.kTool, &r)
	raw, _ := json.Marshal(r)
	return c.HandleNATS(context.Background(), route, raw)
}
func testCaller() tool.Caller {
	return tool.Caller{Subject: "owner", ConnectionID: "rtc1", Origin: "s1", Level: 9, ExpiresAt: time.Now().Add(time.Minute)}
}
func decoded[T any](t *testing.T, v any) T {
	t.Helper()
	raw, _ := json.Marshal(v)
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func TestCatalogContainsOnlyFSAndExec(t *testing.T) {
	c, _ := testClient(t)
	catalog := c.tools.Catalog(context.Background(), testCaller())
	found := map[string]bool{}
	for _, command := range catalog.Exec.Commands {
		found[command.Name] = true
		if command.Help == "" || command.RequiredLevel < 1 {
			t.Fatalf("incomplete declaration: %+v", command)
		}
		for _, method := range command.Methods {
			if method.Mode == wire.Stream {
				t.Fatal("AI catalog includes stream")
			}
		}
	}
	for _, name := range []string{"browser", "cua", "sh", "json", "bg_wait"} {
		if !found[name] {
			t.Fatalf("missing exec command %s", name)
		}
	}
	if len(catalog.FS) == 0 {
		t.Fatal("missing builtin fs")
	}
	raw, _ := json.Marshal(c.buildCaps())
	if strings.Contains(string(raw), `"tools":`) {
		t.Fatal("third capability returned")
	}
	if _, err := c.tools.Translate([]string{"sh", "-c", "echo --literal"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.tools.Translate([]string{"browser", "page.frames"}); err == nil {
		t.Fatal("CLI exposed stream")
	}
}
func TestProcessAndServiceShareExecutionOwnershipAndCancellation(t *testing.T) {
	saved := cfg.Global
	cfg.Global = cfg.NewOptions()
	defer func() { cfg.Global = saved }()
	cfg.Global.ExecPolicy = cfg.PolicyOpen
	c, _ := testClient(t)
	var starts, closed atomic.Int64
	release := make(chan struct{})
	command := tool.DefineCommand("fixture", tool.Bind(tool.Spec{Name: "wait", Access: 1, Background: true}, func(ctx context.Context, caller tool.Caller, _ struct{}) (map[string]int, error) {
		starts.Add(1)
		caller.Output.Write([]byte("waiting\n"))
		select {
		case <-release:
			return map[string]int{"answer": 42}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}))
	command.Close = func() error { closed.Add(1); return nil }
	if err := c.tools.RegisterCommand(command); err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	request := wire.Request{ID: "svc", Action: "call", Argv: []string{"fixture", "wait"}, Execution: &wire.ExecutionOptions{Epoch: c.procs.Epoch(), ID: "once", WaitMS: &zero}, TimeoutMS: 5000}
	result := c.HandleTool(context.Background(), testCaller(), request)
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	job := decoded[exec_procs.Result](t, result.Result)
	if !job.Background {
		t.Fatal("wait did not detach")
	}
	duplicate := c.HandleTool(context.Background(), testCaller(), request)
	if duplicate.Error != nil {
		t.Fatal(duplicate.Error)
	}
	c.DisconnectTools(testCaller())
	close(release)
	done, err := c.procs.Wait(context.Background(), job.ID, time.Second)
	if err != nil || done.Status != "succeeded" || starts.Load() != 1 {
		t.Fatalf("%+v %v starts=%d", done, err, starts.Load())
	}
	value := decoded[map[string]int](t, done.Value)
	if value["answer"] != 42 || !strings.Contains(done.Content, "waiting") {
		t.Fatal("typed result/output lost", done)
	}
	other := testCaller()
	other.Origin = "other"
	if _, err := c.executionControl(context.Background(), other, "bg_wait", []string{job.ID}); err == nil {
		t.Fatal("foreign origin read output")
	}
	process := c.HandleTool(context.Background(), testCaller(), wire.Request{ID: "process", Action: "call", Argv: []string{"sh", "-c", "printf 'start\\n'; sleep 30"}, Execution: &wire.ExecutionOptions{Epoch: c.procs.Epoch(), ID: "process", WaitMS: &zero}, TimeoutMS: 5000})
	if process.Error != nil {
		t.Fatal(process.Error)
	}
	proc := decoded[exec_procs.Result](t, process.Result)
	if _, err := c.executionControl(context.Background(), testCaller(), "bg_kill", []string{proc.ID}); err != nil {
		t.Fatal(err)
	}
	stopped, err := c.procs.Wait(context.Background(), proc.ID, 2*time.Second)
	if err != nil || stopped.Background || stopped.Status != "cancelled" {
		t.Fatalf("process cancel %+v %v", stopped, err)
	}
	if closed.Load() != 0 {
		t.Fatal("bg_kill closed shared service")
	}
}
func TestSignedFSProxySharesFilesystemWithoutSession(t *testing.T) {
	c, _ := testClient(t)
	m := c.buildMgmt()
	if m.Transports["rtc"].Enabled || !m.Transports["proxy"].Supports(natswire.Protocol, "fs") {
		t.Fatal(m)
	}
	path := filepath.Join(c.opts.WorkDir, "note.txt")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	call := func(method string, args any, scope string) wire.Response {
		raw, _ := json.Marshal(args)
		return signedCall(t, c, wire.Request{Action: "call", Call: &wire.Invocation{Domain: "fs", Method: method, Args: raw}}, 9, "", scope)
	}
	location := fsp.Path{RootID: "root", Segments: strings.Split(strings.TrimPrefix(filepath.ToSlash(path), "/"), "/")}
	// Root ID is generated by the device root declaration.
	roots := decoded[struct {
		Roots []struct {
			ID string `json:"id"`
		}
	}](t, call("roots", map[string]any{}, "fs").Result)
	location.RootID = roots.Roots[0].ID
	stat := call("stat", map[string]any{"path": location}, "fs")
	if stat.Error != nil {
		t.Fatal(stat.Error)
	}
	source := decoded[struct {
		Ref fsp.ResourceRef `json:"ref"`
	}](t, call("read", map[string]any{"path": location}, "fs").Result)
	// A new RTC connection uses the same authenticated owner and source.
	rtcCaller := testCaller()
	rtcCaller.Origin = ""
	raw, _ := json.Marshal(map[string]any{"ref": source.Ref, "offset": 0, "length": 8})
	response := c.HandleTool(context.Background(), rtcCaller, wire.Request{ID: "range", Action: "call", Call: &wire.Invocation{Domain: "fs", Method: "source.read", Args: raw}})
	if response.Error != nil {
		t.Fatal(response.Error)
	}
	bytes := decoded[struct {
		Data []byte `json:"data"`
	}](t, response.Result)
	if string(bytes.Data) != "original" {
		t.Fatal(string(bytes.Data))
	}
	// AI text writes go through the same FS version-aware implementation.
	write := call("text.write", map[string]any{"path": path, "content": "changed"}, "fs")
	if write.Error != nil {
		t.Fatal(write.Error)
	}
	text := decoded[vcore.Result](t, write.Result)
	_ = text
	if got, _ := os.ReadFile(path); string(got) != "changed" {
		t.Fatal(string(got))
	}
	reread := call("source.read", map[string]any{"ref": source.Ref, "offset": 0, "length": 8}, "fs")
	// Replacing the path preserves the opened old inode; it cannot read a new file.
	if reread.Error == nil {
		got := decoded[struct {
			Data []byte `json:"data"`
		}](t, reread.Result)
		if string(got.Data) != "original" {
			t.Fatal("source silently retargeted")
		}
	}
	forbidden := signedCall(t, c, wire.Request{Action: "call", Argv: []string{"browser", "pages"}}, 9, "", "fs")
	if forbidden.Error == nil {
		t.Fatal("fs proxy invoked exec")
	}
}

func TestRTCCatalogFitsMessageBudget(t *testing.T) {
	c, _ := testClient(t)
	caller := testCaller()
	caller.AllowStreams = true
	for _, q := range []*wire.CatalogQuery{nil, {Domain: "fs"}, {Domain: "exec", Command: "browser"}, {Domain: "exec", Command: "cua"}} {
		r := c.HandleTool(context.Background(), caller, wire.Request{Protocol: "hosts_rtc/1", ID: "catalog", Action: "catalog", Catalog: q})
		raw, err := json.Marshal(r)
		if err != nil || r.Error != nil {
			t.Fatal(err, r.Error)
		}
		t.Logf("RTC catalog %v: %d bytes", q, len(raw))
		if len(raw) > 64<<10 {
			t.Fatal("catalog exceeds minimum RTC message budget")
		}
	}
}

func TestExecutionRejectsOldEpochAndReducedGrant(t *testing.T) {
	c, _ := testClient(t)
	command := tool.DefineCommand("fixture", tool.Bind(tool.Spec{Name: "wait", Access: 3, Background: true}, func(context.Context, tool.Caller, struct{}) (bool, error) { return true, nil }))
	if err := c.tools.RegisterCommand(command); err != nil {
		t.Fatal(err)
	}
	req := wire.Request{ID: "id", Action: "call", Argv: []string{"fixture", "wait"}, Execution: &wire.ExecutionOptions{Epoch: "previous", ID: "once"}}
	if r := c.HandleTool(context.Background(), testCaller(), req); r.Error == nil || r.Error.Code != "expired" {
		t.Fatal(r)
	}
	req.Execution.Epoch = c.procs.Epoch()
	r := c.HandleTool(context.Background(), testCaller(), req)
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	result := decoded[exec_procs.Result](t, r.Result)
	low := testCaller()
	low.Level = 1
	if _, err := c.executionControl(context.Background(), low, "bg_wait", []string{result.ID}); err == nil {
		t.Fatal("reduced grant read result")
	}
}
