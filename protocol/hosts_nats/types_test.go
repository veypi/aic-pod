package hosts_nats

import (
	"encoding/json"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
	"testing"
	"time"
)

func TestSignatureBindsEveryExecutionField(t *testing.T) {
	subject, _ := Subject("u1", "h1")
	r := Request{HostID: "h1", Subject: subject, Caller: "u1", GrantedLevel: 2, Nonce: "nonce", Deadline: time.Now().Add(time.Minute).UnixMilli(), AuthorizationUntil: time.Now().Add(2 * time.Minute).UnixMilli(), Request: wire.Request{Protocol: Protocol, ID: "req", Action: "call", Call: &wire.Invocation{Domain: "exec", Command: "browser", Method: "page.list", Args: json.RawMessage(`{}`)}}}
	Sign("secret", &r)
	if err := Verify("secret", "h1", subject, r, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Request){func(r *Request) { r.Scope = "fs" }, func(r *Request) { r.AuthorizationUntil += 1 }, func(r *Request) { r.Request.Execution = &wire.ExecutionOptions{Epoch: "epoch", ID: "exec"} }, func(r *Request) { r.Caller = "u2" }, func(r *Request) { r.Origin = "another" }, func(r *Request) { r.GrantedLevel = 9 }, func(r *Request) { r.Request.TimeoutMS = 5000 }, func(r *Request) { r.Subject += ".other" }, func(r *Request) { r.Request.Call.Args = json.RawMessage(`{"page_id":"another"}`) }} {
		copy := r
		call := *r.Request.Call
		copy.Request.Call = &call
		change(&copy)
		if Verify("secret", "h1", subject, copy, time.Now()) == nil {
			t.Fatal("tampering accepted")
		}
	}
}

// 对时校准误差容差：零容差时「设备校准时间略慢于平台」会把整段会话的请求全部
// 拒掉；Deadline 上限也需覆盖平台工具请求最长等待（10min）而非 5min。
func TestVerifyClockSlackToleratesCalibrationError(t *testing.T) {
	subject, _ := Subject("u1", "h1")
	base := time.Now()
	mk := func(deadline, authorization time.Time) Request {
		r := Request{HostID: "h1", Subject: subject, Caller: "u1", GrantedLevel: 2, Nonce: "nonce", Deadline: deadline.UnixMilli(), AuthorizationUntil: authorization.UnixMilli(), Request: wire.Request{Protocol: Protocol, ID: "req", Action: "call", Call: &wire.Invocation{Domain: "exec", Command: "browser", Method: "page.list", Args: json.RawMessage(`{}`)}}}
		Sign("secret", &r)
		return r
	}
	// 误差负侧：平台零余量签发 now+30min，设备校准时间慢 1min（零容差会全拒）。
	r := mk(base.Add(time.Minute), base.Add(30*time.Minute))
	if err := Verify("secret", "h1", subject, r, base.Add(-time.Minute)); err != nil {
		t.Fatal("clock slack (authorization) rejected: ", err)
	}
	// 误差正侧：Deadline 已过 1min，容差内仍接受。
	r = mk(base, base.Add(30*time.Minute))
	if err := Verify("secret", "h1", subject, r, base.Add(time.Minute)); err != nil {
		t.Fatal("clock slack (deadline) rejected: ", err)
	}
	// 长等待：平台 timeout 上限 10min（Deadline=now+timeout），5min 上限时代必然被拒。
	r = mk(base.Add(8*time.Minute), base.Add(30*time.Minute))
	if err := Verify("secret", "h1", subject, r, base); err != nil {
		t.Fatal("long deadline rejected: ", err)
	}
	// 超窗仍拒：授权 40min、Deadline 20min 均超界。
	r = mk(base.Add(20*time.Minute), base.Add(40*time.Minute))
	if err := Verify("secret", "h1", subject, r, base); err == nil {
		t.Fatal("oversized validity windows accepted")
	}
}
