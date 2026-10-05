package host

import (
	"context"
	"encoding/json"
	"github.com/veypi/aic-pod/protocol"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/execution"
	"github.com/veypi/aic-pod/libs/fsx"
)

func TestExecNativeEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh as an external probe")
	}
	saved := cfg.Global
	cfg.Global = cfg.NewOptions()
	t.Cleanup(func() { cfg.Global = saved })
	t.Setenv("AIC_TEST_ENV_DEMO", "inherited")
	c, _ := testClient(t)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "aic-env-probe"), []byte("#!/bin/sh\nprintf '%s' \"$AIC_TEST_ENV_DEMO\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, script, want string }{
		{"export", `export AIC_TEST_ENV_DEMO=exported; /bin/sh -c 'printf %s "$AIC_TEST_ENV_DEMO"'`, "exported"},
		{"inline", `AIC_TEST_ENV_DEMO=inline /bin/sh -c 'printf %s "$AIC_TEST_ENV_DEMO"'`, "inline"},
		{"inherited-reassignment", `AIC_TEST_ENV_DEMO=local; /bin/sh -c 'printf %s "$AIC_TEST_ENV_DEMO"'`, "local"},
		{"unexport", `export -n AIC_TEST_ENV_DEMO; AIC_TEST_ENV_DEMO=local; /bin/sh -c 'printf %s "$AIC_TEST_ENV_DEMO"'`, ""},
		{"unset", `unset AIC_TEST_ENV_DEMO; /bin/sh -c 'printf %s "$AIC_TEST_ENV_DEMO"'`, ""},
		{"clear-env", `env -i /bin/sh -c 'printf %s "$AIC_TEST_ENV_DEMO"'`, ""},
		{"empty", `AIC_TEST_ENV_DEMO= /bin/sh -c 'printf %s "$AIC_TEST_ENV_DEMO"'`, ""},
		{"child-path", `/bin/sh -c 'ls /dev/null'`, "/dev/null\n"},
		{"inline-path", "PATH='" + hostCanonical(bin) + "' AIC_TEST_ENV_DEMO=custom aic-env-probe", "custom"},
		{"request-isolation", `/bin/sh -c 'printf %s "$AIC_TEST_ENV_DEMO"'`, "inherited"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := callTool(t, c, context.Background(), testCaller(), execRequest(tc.script, 30000))
			attrs := execAttrs(t, r)
			if attrs["exit_code"] != "0" {
				t.Fatalf("exec: %+v", r)
			}
			data, err := os.ReadFile(attrs["output"])
			if err != nil || string(data) != tc.want {
				t.Fatalf("stdout=%q, err=%v; want %q", data, err, tc.want)
			}
		})
	}
}

func testClient(t *testing.T) (*Client, string) {
	t.Helper()
	// 隔离设备状态目录，测试不得写开发者的真实 HOME。
	t.Setenv("HOME", t.TempDir())
	c := New(Options{Key: "host_1.1.secret.owner", WorkDir: t.TempDir(), NoSandbox: true})
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
func signedCall(t *testing.T, c *Client, req protocol.Request, grantApproved bool, origin, scope string) protocol.Response {
	t.Helper()
	if req.ID == "" {
		req.ID = protocol.NewID("r_")
	}
	route, _ := protocol.NatsSubject(c.uid, c.hostID)
	r := protocol.NatsRequest{HostID: c.hostID, Subject: route, Caller: c.uid, Origin: origin, Scope: scope, GrantApproved: grantApproved, Nonce: protocol.NewID("n_"), Deadline: time.Now().Add(time.Minute).UnixMilli(), AuthorizationUntil: time.Now().Add(2 * time.Minute).UnixMilli(), Request: req}
	protocol.NatsSign(c.kTool, &r)
	raw, _ := json.Marshal(r)
	return callNATS(t, c, route, raw)
}
func testCaller() protocol.Caller {
	return protocol.Caller{Subject: "owner", ConnectionID: "rtc1", Origin: "s1", ExpiresAt: time.Now().Add(time.Minute)}
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

func fsRequest(method string, args any) protocol.Request {
	raw, _ := json.Marshal(args)
	return protocol.Request{Protocol: protocol.NatsProtocol, ID: protocol.NewID("r_"), Action: protocol.ActionFS, FS: &protocol.FSInvocation{Method: method, Args: raw}}
}
func execRequest(script string, waitMS int64) protocol.Request {
	return protocol.Request{Protocol: protocol.NatsProtocol, ID: protocol.NewID("r_"), Action: protocol.ActionExec, Exec: &protocol.ExecPayload{Script: script, WaitMS: waitMS}}
}

// execResult 解码统一 exec 输出（§3.1：content + attrs）。
func execAttrs(t *testing.T, r protocol.Response) map[string]string {
	t.Helper()
	if r.Error != nil {
		t.Fatalf("exec failed: %+v", r.Error)
	}
	res := decoded[protocol.Output](t, r.Result)
	return res.Attrs
}

func TestExecScriptForeground(t *testing.T) {
	saved := cfg.Global
	cfg.Global = cfg.NewOptions()
	t.Cleanup(func() { cfg.Global = saved })
	c, _ := testClient(t)
	r := callTool(t, c, context.Background(), testCaller(), execRequest("echo hello-vsh", 30000))
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	res := decoded[protocol.Output](t, r.Result)
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
	res := decoded[protocol.Output](t, r.Result)
	if res.Attrs["background"] != "true" || res.Attrs["id"] == "" {
		t.Fatalf("not backgrounded: %v", res.Attrs)
	}
	// cancel(request_id) 终止同一执行（§2.6）。
	cancel := c.HandleTool(context.Background(), caller, protocol.Request{ID: protocol.NewID("r_"), Action: protocol.ActionCancel, CancelID: req.ID})
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
	r2 := c.HandleTool(context.Background(), stranger, protocol.Request{ID: protocol.NewID("r_"), Action: protocol.ActionCancel, CancelID: req.ID})
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
	r := callTool(t, c, context.Background(), testCaller(), execRequest("grant fs "+target, 30000))
	res := decoded[protocol.Output](t, r.Result)
	if res.Attrs["exit_code"] == "0" {
		t.Fatalf("grant without grant_approved succeeded: %q", res.Content)
	}
	if !strings.Contains(res.Attrs["stderr"], "grant_approved") {
		t.Fatalf("stderr lacks guidance: %q", res.Attrs["stderr"])
	}
	// 携带审批事实（签名信封/RTC 确认）：授权执行。
	approved := testCaller()
	approved.GrantApproved = true
	r = callTool(t, c, context.Background(), approved, execRequest("grant fs "+target, 30000))
	res = decoded[protocol.Output](t, r.Result)
	if res.Attrs["exit_code"] != "0" {
		t.Fatalf("approved grant failed: %q stderr=%q", res.Content, res.Attrs["stderr"])
	}
	if !c.perms.fsSnapshot("s1").Match(filepath.ToSlash(target)+"/x", 1).Allow {
		t.Fatal("grant not effective")
	}
}

func TestSignedFSProxySharesFilesystemWithoutSession(t *testing.T) {
	c, _ := testClient(t)
	m := c.buildMgmt()
	if m.Transports["rtc"].Enabled || !m.Transports["proxy"].Supports(protocol.NatsProtocol, "fs") {
		t.Fatal(m)
	}
	path := filepath.Join(c.opts.WorkDir, "note.txt")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	call := func(method string, args any, scope string) protocol.Response {
		return signedCall(t, c, fsRequest(method, args), false, "", scope)
	}
	location := protocol.FSPath{RootID: "root", Segments: strings.Split(strings.TrimPrefix(filepath.ToSlash(path), "/"), "/")}
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
		Ref protocol.FSResourceRef `json:"ref"`
	}](t, call("read", map[string]any{"path": location}, "fs").Result)
	// A new RTC connection uses the same authenticated owner and source.
	rtcCaller := testCaller()
	rtcCaller.Origin = ""
	response := callTool(t, c, context.Background(), rtcCaller, fsRequest("source.read", map[string]any{"ref": source.Ref, "offset": 0, "length": 8}))
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

// execResultLoose 解码错误响应中保留的部分结果（Reply 保留 Result——
// 错误与部分结果并存，attrs 恒含日志地址）。
func execResultLoose(t *testing.T, r protocol.Response) protocol.Output {
	t.Helper()
	if r.Result == nil {
		t.Fatalf("错误响应丢失了部分结果（日志地址）: %+v", r.Error)
	}
	return decoded[protocol.Output](t, r.Result)
}

// waitRunDone 等 requestID 对应执行实际结束（取消/断连是异步的，测试
// 返回前必须等执行 goroutine 写完日志，否则与 TempDir 清理竞争）。
func waitRunDone(t *testing.T, c *Client, requestID string) {
	t.Helper()
	engine, err := c.engine()
	if err != nil {
		t.Fatal(err)
	}
	if !engine.Tasks.WaitRun(requestID, 10*time.Second) {
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
	caller.Direct = true // RTC 直连语义：等待超时返回错误、不转 bg
	req := execRequest("sleep 30", 50)
	r := callTool(t, c, context.Background(), caller, req)
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
	// 执行仍在运行：断连取消前台执行并清理。
	c.DisconnectTools(caller)
	waitRunDone(t, c, req.ID)
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
	engine.Tasks = execution.NewTaskTableWithCaps(1, 1)
	caller := testCaller()
	// 第一次执行等待超时转后台，占满唯一名额。
	first := execRequest("sleep 30", 50)
	r1 := callTool(t, c, context.Background(), caller, first)
	if r1.Error != nil {
		t.Fatal(r1.Error)
	}
	id1 := decoded[protocol.Output](t, r1.Result).Attrs["id"]
	if decoded[protocol.Output](t, r1.Result).Attrs["background"] != "true" || id1 == "" {
		t.Fatal("第一次执行未转后台占位")
	}
	defer func() {
		callTool(t, c, context.Background(), caller, execRequest("bg kill "+id1, 30000))
		// 等占位任务实际结束（取消是异步的），再交 TempDir 清理。
		if _, err := engine.Tasks.Wait(context.Background(), id1, 10*time.Second, "owner", "s1"); err != nil {
			t.Errorf("占位任务未结束: %v", err)
		}
	}()
	// 第二次执行转后台容量不足：取消本次执行并返回 overloaded + 日志路径。
	second := execRequest("sleep 30", 50)
	r2 := callTool(t, c, context.Background(), caller, second)
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
	waitRunDone(t, c, second.ID)
}

// DisconnectTools 断连清理：取消该连接发起的前台执行（后台任务不受影响）。
func TestDisconnectToolsCancelsForeground(t *testing.T) {
	saved := cfg.Global
	cfg.Global = cfg.NewOptions()
	t.Cleanup(func() { cfg.Global = saved })
	c, _ := testClient(t)
	caller := testCaller()
	req := execRequest("sleep 30; echo SURVIVED", 30000)
	type reply struct{ r protocol.Response }
	done := make(chan reply, 1)
	start := time.Now()
	go func() { done <- reply{callTool(t, c, context.Background(), caller, req)} }()
	time.Sleep(300 * time.Millisecond)
	c.DisconnectTools(caller)
	var r protocol.Response
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
	waitRunDone(t, c, req.ID)
}

func TestCancelUnknownExecution(t *testing.T) {
	c, _ := testClient(t)
	r := c.HandleTool(context.Background(), testCaller(), protocol.Request{ID: protocol.NewID("r_"), Action: protocol.ActionCancel, CancelID: "r_nonexistent"})
	if r.Error == nil || r.Error.Code != "not_found" {
		t.Fatalf("%+v", r)
	}
}

func TestExecScriptForegroundCancel(t *testing.T) {
	saved := cfg.Global
	cfg.Global = cfg.NewOptions()
	t.Cleanup(func() { cfg.Global = saved })
	c, _ := testClient(t)
	caller := testCaller()
	req := execRequest("sleep 30; echo SURVIVED", 30000)
	type reply struct{ r protocol.Response }
	done := make(chan reply, 1)
	start := time.Now()
	go func() { done <- reply{c.HandleTool(context.Background(), caller, req)} }()
	time.Sleep(300 * time.Millisecond)
	cancel := c.HandleTool(context.Background(), caller, protocol.Request{ID: protocol.NewID("r_"), Action: protocol.ActionCancel, CancelID: req.ID})
	if cancel.Error != nil {
		t.Fatal(cancel.Error)
	}
	var r protocol.Response
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
