package host

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/fsx"
	tool "github.com/veypi/aic-pod/libs/hosts_tool"
	vshglue "github.com/veypi/aic-pod/libs/vsh"
	fsp "github.com/veypi/aic-pod/protocol/fs"
	natswire "github.com/veypi/aic-pod/protocol/hosts_nats"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
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

// signedCall 经 NATS 可信通道下发请求（hosts_nats/2：grantApproved 是签名
// 信封内的审批事实，随信封进可信调用上下文）。
func signedCall(t *testing.T, c *Client, req wire.Request, grantApproved bool, origin, scope string) wire.Response {
	t.Helper()
	req.Protocol = natswire.Protocol
	if req.ID == "" {
		req.ID = wire.NewID("r_")
	}
	route, _ := natswire.Subject(c.uid, c.hostID)
	r := natswire.Request{HostID: c.hostID, Subject: route, Caller: c.uid, Origin: origin, Scope: scope, GrantApproved: grantApproved, Nonce: wire.NewID("n_"), Deadline: time.Now().Add(time.Minute).UnixMilli(), AuthorizationUntil: time.Now().Add(2 * time.Minute).UnixMilli(), Request: req}
	natswire.Sign(c.kTool, &r)
	raw, _ := json.Marshal(r)
	return c.HandleNATS(context.Background(), route, raw)
}
func testCaller() tool.Caller {
	return tool.Caller{Subject: "owner", ConnectionID: "rtc1", Origin: "s1", ExpiresAt: time.Now().Add(time.Minute)}
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

func fsRequest(method string, args any) wire.Request {
	raw, _ := json.Marshal(args)
	return wire.Request{ID: wire.NewID("r_"), Action: wire.ActionFS, FS: &wire.FSInvocation{Method: method, Args: raw}}
}

func execRequest(script string, waitMS int64) wire.Request {
	return wire.Request{ID: wire.NewID("r_"), Action: wire.ActionExec, Exec: &wire.ExecPayload{Script: script, WaitMS: waitMS}}
}

// execResult 解码统一 exec 输出（§3.1：content + attrs）。
func execAttrs(t *testing.T, r wire.Response) map[string]string {
	t.Helper()
	if r.Error != nil {
		t.Fatalf("exec failed: %+v", r.Error)
	}
	res := decoded[wire.ExecResult](t, r.Result)
	return res.Attrs
}

func TestExecScriptForeground(t *testing.T) {
	saved := cfg.Global
	cfg.Global = cfg.NewOptions()
	t.Cleanup(func() { cfg.Global = saved })
	c, _ := testClient(t)
	r := c.HandleTool(context.Background(), testCaller(), execRequest("echo hello-vsh", 30000))
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	res := decoded[wire.ExecResult](t, r.Result)
	if !strings.Contains(res.Content, "hello-vsh") {
		t.Fatalf("content = %q", res.Content)
	}
	if res.Attrs["exit_code"] != "0" || res.Attrs["action"] != "exec" {
		t.Fatalf("attrs = %v", res.Attrs)
	}
	// 双流日志落盘（§2.5）：stdout 全量在 output 路径。
	logOut, logErr := res.Attrs["output"], res.Attrs["error_output"]
	if logOut == "" || logErr == "" {
		t.Fatalf("missing log paths: %v", res.Attrs)
	}
	data, err := os.ReadFile(logOut)
	if err != nil || !strings.Contains(string(data), "hello-vsh") {
		t.Fatalf("stdout log = %q, %v", data, err)
	}
	if _, err := os.Stat(logErr); err != nil {
		t.Fatalf("stderr log missing: %v", err)
	}
}

func TestExecScriptTimeoutAdoptsBackgroundThenCancel(t *testing.T) {
	saved := cfg.Global
	cfg.Global = cfg.NewOptions()
	t.Cleanup(func() { cfg.Global = saved })
	c, _ := testClient(t)
	caller := testCaller()
	req := execRequest("sleep 30", 50)
	r := c.HandleTool(context.Background(), caller, req)
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	res := decoded[wire.ExecResult](t, r.Result)
	if res.Attrs["background"] != "true" || res.Attrs["id"] == "" {
		t.Fatalf("not backgrounded: %v", res.Attrs)
	}
	// cancel(request_id) 终止同一执行（§2.6）。
	cancel := c.HandleTool(context.Background(), caller, wire.Request{ID: wire.NewID("r_"), Action: wire.ActionCancel, CancelID: req.ID})
	if cancel.Error != nil {
		t.Fatal(cancel.Error)
	}
	engine, err := c.engine()
	if err != nil {
		t.Fatal(err)
	}
	task, ok := engine.Tasks.Get(res.Attrs["id"], "owner", "s1")
	if !ok {
		t.Fatalf("task missing: %s", res.Attrs["id"])
	}
	done, err := engine.Tasks.Wait(context.Background(), task.ID, 5*time.Second, "owner", "s1")
	if err != nil || done.Status != "killed" {
		t.Fatalf("task = %+v, %v", done, err)
	}
	// 归属不匹配 = 不存在（取消不跨用户/会话）。
	stranger := testCaller()
	stranger.Origin = "other"
	r2 := c.HandleTool(context.Background(), stranger, wire.Request{ID: wire.NewID("r_"), Action: wire.ActionCancel, CancelID: req.ID})
	if r2.Error == nil || r2.Error.Code != "not_found" {
		t.Fatalf("cross-session cancel admitted: %+v", r2)
	}
}

func TestExecGrantRequiresGrantApproved(t *testing.T) {
	saved := cfg.Global
	cfg.Global = cfg.NewOptions()
	t.Cleanup(func() { cfg.Global = saved })
	c, _ := testClient(t)
	target := t.TempDir()
	// 未携带审批事实：grant 修改入口拒绝（退出码非 0，stderr 引导）。
	r := c.HandleTool(context.Background(), testCaller(), execRequest("grant fs "+target, 30000))
	res := decoded[wire.ExecResult](t, r.Result)
	if res.Attrs["exit_code"] == "0" {
		t.Fatalf("grant without grant_approved succeeded: %q", res.Content)
	}
	if !strings.Contains(res.Attrs["stderr"], "grant_approved") {
		t.Fatalf("stderr lacks guidance: %q", res.Attrs["stderr"])
	}
	// 携带审批事实（签名信封/RTC 确认）：授权执行。
	approved := testCaller()
	approved.GrantApproved = true
	r = c.HandleTool(context.Background(), approved, execRequest("grant fs "+target, 30000))
	res = decoded[wire.ExecResult](t, r.Result)
	if res.Attrs["exit_code"] != "0" {
		t.Fatalf("approved grant failed: %q stderr=%q", res.Content, res.Attrs["stderr"])
	}
	if !c.policy.Snapshot("s1").Match(filepath.ToSlash(target)+"/x", 1).Allow {
		t.Fatal("grant not effective")
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
		return signedCall(t, c, fsRequest(method, args), false, "", scope)
	}
	location := fsp.Path{RootID: "root", Segments: strings.Split(strings.TrimPrefix(filepath.ToSlash(path), "/"), "/")}
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
	response := c.HandleTool(context.Background(), rtcCaller, fsRequest("source.read", map[string]any{"ref": source.Ref, "offset": 0, "length": 8}))
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
	text := decoded[fsx.Result](t, write.Result)
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
	// fs-only 范围不能借文件入口执行命令（§4.4）。
	forbidden := signedCall(t, c, execRequest("echo no", 1000), false, "", "fs")
	if forbidden.Error == nil || forbidden.Error.Code != "permission_denied" {
		t.Fatalf("fs proxy invoked exec: %+v", forbidden)
	}
}

func TestCancelUnknownExecution(t *testing.T) {
	c, _ := testClient(t)
	r := c.HandleTool(context.Background(), testCaller(), wire.Request{ID: wire.NewID("r_"), Action: wire.ActionCancel, CancelID: "r_nonexistent"})
	if r.Error == nil || r.Error.Code != "not_found" {
		t.Fatalf("%+v", r)
	}
}

// execResultLoose 解码错误响应中保留的部分结果（Reply 保留 Result——
// 错误与部分结果并存，attrs 恒含日志地址）。
func execResultLoose(t *testing.T, r wire.Response) wire.ExecResult {
	t.Helper()
	if r.Result == nil {
		t.Fatalf("错误响应丢失了部分结果（日志地址）: %+v", r.Error)
	}
	return decoded[wire.ExecResult](t, r.Result)
}

// trackedExec 取前台执行的登记句柄（测试等待执行实际结束用——取消/断连
// 是异步的，测试返回前必须等执行 goroutine 写完 stub/日志，否则与
// TempDir 清理竞争）。
func trackedExec(t *testing.T, c *Client, requestID string) *execHandleEntry {
	t.Helper()
	c.execMu.Lock()
	defer c.execMu.Unlock()
	return c.execHandles[requestID]
}

func waitHandleDone(t *testing.T, h *vshglue.ExecHandle) {
	t.Helper()
	select {
	case <-h.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("执行未在预期内结束")
	}
}

// RTC 前台等待超时（deadline_exceeded）：错误响应仍含 attrs.output/
// error_output 日志地址（执行继续，cancel(request_id) 可终止）。
func TestExecScriptRTCWaitElapsedKeepsLogs(t *testing.T) {
	saved := cfg.Global
	cfg.Global = cfg.NewOptions()
	t.Cleanup(func() { cfg.Global = saved })
	c, _ := testClient(t)
	caller := testCaller()
	caller.AllowStreams = true // RTC 直连语义：等待超时返回错误、不转 bg
	req := execRequest("sleep 30", 50)
	r := c.HandleTool(context.Background(), caller, req)
	if r.Error == nil || r.Error.Code != "deadline_exceeded" {
		t.Fatalf("resp = %+v", r)
	}
	res := execResultLoose(t, r)
	logOut, logErr := res.Attrs["output"], res.Attrs["error_output"]
	if logOut == "" || logErr == "" {
		t.Fatalf("missing log paths: %v", res.Attrs)
	}
	if _, err := os.Stat(logOut); err != nil {
		t.Fatalf("stdout log missing: %v", err)
	}
	// 执行仍在运行：cancel(request_id) 终止并清理。
	cancel := c.HandleTool(context.Background(), caller, wire.Request{ID: wire.NewID("r_"), Action: wire.ActionCancel, CancelID: req.ID})
	if cancel.Error != nil {
		t.Fatal(cancel.Error)
	}
	if e := trackedExec(t, c, req.ID); e != nil {
		waitHandleDone(t, e.handle)
	}
}

// 转后台容量不足取消：错误码为资源类 overloaded，错误响应仍含日志路径；
// 未登记的执行不游离运行（任务表只有占位任务）。
func TestExecScriptCapacityCancelKeepsLogs(t *testing.T) {
	saved := cfg.Global
	cfg.Global = cfg.NewOptions()
	t.Cleanup(func() { cfg.Global = saved })
	c, _ := testClient(t)
	engine, err := c.engine()
	if err != nil {
		t.Fatal(err)
	}
	engine.Tasks = vshglue.NewTaskTableWithCaps(1, 1)
	caller := testCaller()
	// 第一次执行等待超时转后台，占满唯一名额。
	first := execRequest("sleep 30", 50)
	r1 := c.HandleTool(context.Background(), caller, first)
	if r1.Error != nil {
		t.Fatal(r1.Error)
	}
	id1 := decoded[wire.ExecResult](t, r1.Result).Attrs["id"]
	if decoded[wire.ExecResult](t, r1.Result).Attrs["background"] != "true" || id1 == "" {
		t.Fatal("第一次执行未转后台占位")
	}
	defer func() {
		c.HandleTool(context.Background(), caller, wire.Request{ID: wire.NewID("r_"), Action: wire.ActionCancel, CancelID: first.ID})
		// 等占位任务实际结束（取消是异步的），再交 TempDir 清理。
		if _, err := engine.Tasks.Wait(context.Background(), id1, 10*time.Second, "owner", "s1"); err != nil {
			t.Errorf("占位任务未结束: %v", err)
		}
	}()
	// 第二次执行转后台容量不足：取消本次执行并返回 overloaded + 日志路径。
	second := execRequest("sleep 30", 50)
	r2 := c.HandleTool(context.Background(), caller, second)
	if r2.Error == nil || r2.Error.Code != "overloaded" {
		t.Fatalf("resp = %+v", r2)
	}
	res := execResultLoose(t, r2)
	logOut, logErr := res.Attrs["output"], res.Attrs["error_output"]
	if logOut == "" || logErr == "" {
		t.Fatalf("missing log paths: %v", res.Attrs)
	}
	if _, err := os.Stat(logOut); err != nil {
		t.Fatalf("stdout log missing: %v", err)
	}
	if _, err := os.Stat(logErr); err != nil {
		t.Fatalf("stderr log missing: %v", err)
	}
	if got := engine.Tasks.List("owner", "s1"); len(got) != 1 {
		t.Fatalf("容量不足的执行不应登记后台: %v", got)
	}
	// 容量取消是异步的：等被取消的执行 goroutine 实际结束。
	if e := trackedExec(t, c, second.ID); e != nil {
		waitHandleDone(t, e.handle)
	}
}

// 前台 cancel 路径：exec 阻塞中由另一 goroutine 发 cancel(request_id)，
// 脚本被终止、响应带日志地址（§2.6）。
func TestExecScriptForegroundCancel(t *testing.T) {
	saved := cfg.Global
	cfg.Global = cfg.NewOptions()
	t.Cleanup(func() { cfg.Global = saved })
	c, _ := testClient(t)
	caller := testCaller()
	req := execRequest("sleep 30; echo SURVIVED", 30000)
	type reply struct{ r wire.Response }
	done := make(chan reply, 1)
	start := time.Now()
	go func() { done <- reply{c.HandleTool(context.Background(), caller, req)} }()
	time.Sleep(300 * time.Millisecond)
	cancel := c.HandleTool(context.Background(), caller, wire.Request{ID: wire.NewID("r_"), Action: wire.ActionCancel, CancelID: req.ID})
	if cancel.Error != nil {
		t.Fatal(cancel.Error)
	}
	var r wire.Response
	select {
	case got := <-done:
		r = got.r
	case <-time.After(10 * time.Second):
		t.Fatal("cancel 未终止前台执行（sleep 30 跑满）")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("脚本未被终止")
	}
	res := execResultLoose(t, r)
	if strings.Contains(res.Content, "SURVIVED") {
		t.Fatal("脚本在 cancel 后仍跑完")
	}
	logOut, logErr := res.Attrs["output"], res.Attrs["error_output"]
	if logOut == "" || logErr == "" {
		t.Fatalf("missing log paths: %v", res.Attrs)
	}
	if _, err := os.Stat(logOut); err != nil {
		t.Fatalf("stdout log missing: %v", err)
	}
}

// DisconnectTools 断连清理：取消该连接发起的前台执行（后台任务不受影响）。
func TestDisconnectToolsCancelsForeground(t *testing.T) {
	saved := cfg.Global
	cfg.Global = cfg.NewOptions()
	t.Cleanup(func() { cfg.Global = saved })
	c, _ := testClient(t)
	caller := testCaller()
	req := execRequest("sleep 30; echo SURVIVED", 30000)
	type reply struct{ r wire.Response }
	done := make(chan reply, 1)
	start := time.Now()
	go func() { done <- reply{c.HandleTool(context.Background(), caller, req)} }()
	time.Sleep(300 * time.Millisecond)
	entry := trackedExec(t, c, req.ID)
	if entry == nil {
		t.Fatal("前台执行未登记取消句柄")
	}
	c.DisconnectTools(caller)
	var r wire.Response
	select {
	case got := <-done:
		r = got.r
	case <-time.After(10 * time.Second):
		t.Fatal("断连清理未终止前台执行（sleep 30 跑满）")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("脚本未被终止")
	}
	res := execResultLoose(t, r)
	if strings.Contains(res.Content, "SURVIVED") {
		t.Fatal("脚本在断连清理后仍跑完")
	}
	if res.Attrs["output"] == "" || res.Attrs["error_output"] == "" {
		t.Fatalf("missing log paths: %v", res.Attrs)
	}
	waitHandleDone(t, entry.handle)
}
