package hosts_nats

import (
	"testing"
	"time"

	wire "github.com/veypi/aic-pod/protocol/tool"
)

func execReq() wire.Request {
	return wire.Request{Protocol: Protocol, ID: "req", Action: wire.ActionExec, Exec: &wire.ExecPayload{Script: "echo hello"}}
}

// hosts_nats/2：签名覆盖完整请求（含 grant_approved 标记与脚本正文）——
// 篡改任一字段验签必失败（hosts-vsh-redesign §4.2）。
func TestSignatureBindsEveryExecutionField(t *testing.T) {
	subject, _ := Subject("u1", "h1")
	r := Request{HostID: "h1", Subject: subject, Caller: "u1", Nonce: "nonce", Deadline: time.Now().Add(time.Minute).UnixMilli(), AuthorizationUntil: time.Now().Add(2 * time.Minute).UnixMilli(), Request: execReq()}
	Sign("secret", &r)
	if err := Verify("secret", "h1", subject, r, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Request){
		func(r *Request) { r.Scope = "fs" },
		func(r *Request) { r.AuthorizationUntil += 1 },
		func(r *Request) { r.Caller = "u2" },
		func(r *Request) { r.Origin = "another" },
		func(r *Request) { r.GrantApproved = true }, // 篡改审批标记必须验签失败
		func(r *Request) { r.Request.Protocol = "wrong" },
		func(r *Request) { r.Subject += ".other" },
		func(r *Request) { r.Request.Exec = &wire.ExecPayload{Script: "echo altered"} },
	} {
		copy := r
		change(&copy)
		if Verify("secret", "h1", subject, copy, time.Now()) == nil {
			t.Fatal("tampering accepted")
		}
	}
	// grant_approved 合法置位（服务端审批后签发）可验签通过
	r.GrantApproved = true
	Sign("secret", &r)
	if err := Verify("secret", "h1", subject, r, time.Now()); err != nil {
		t.Fatal("signed grant_approved request rejected:", err)
	}
}

// 对时校准误差容差：零容差时「设备校准时间略慢于平台」会把整段会话的请求全部
// 拒掉；Deadline 上限也需覆盖平台工具请求最长等待（10min）而非 5min。
func TestVerifyClockSlackToleratesCalibrationError(t *testing.T) {
	subject, _ := Subject("u1", "h1")
	base := time.Now()
	mk := func(deadline, authorization time.Time) Request {
		r := Request{HostID: "h1", Subject: subject, Caller: "u1", Nonce: "nonce", Deadline: deadline.UnixMilli(), AuthorizationUntil: authorization.UnixMilli(), Request: execReq()}
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

// Caller 必须与签名 subject 目的地中的 uid 一致：执行归属统一从
// caller.Subject 派生，信封 Caller 不能与服务端路由归属脱节（重新签名
// 也必须拒绝）。
func TestVerifyCallerMustMatchSubjectUID(t *testing.T) {
	subject, _ := Subject("u1", "h1")
	r := Request{HostID: "h1", Subject: subject, Caller: "u1", Nonce: "nonce", Deadline: time.Now().Add(time.Minute).UnixMilli(), AuthorizationUntil: time.Now().Add(2 * time.Minute).UnixMilli(), Request: execReq()}
	Sign("secret", &r)
	if err := Verify("secret", "h1", subject, r, time.Now()); err != nil {
		t.Fatal(err)
	}
	// 同一 subject、另一 Caller——即使信封自洽签名也拒绝。
	r.Caller = "u2"
	Sign("secret", &r)
	if err := Verify("secret", "h1", subject, r, time.Now()); err == nil {
		t.Fatal("caller mismatched with subject uid accepted")
	}
}

func TestRequestRejectsMCPAction(t *testing.T) {
	subject, _ := Subject("u1", "h1")
	r := Request{HostID: "h1", Subject: subject, Caller: "u1", Nonce: "n", Deadline: time.Now().Add(time.Minute).UnixMilli(), AuthorizationUntil: time.Now().Add(2 * time.Minute).UnixMilli(), Request: execReq()}
	r.Request.Action = "mcp"
	Sign("key", &r)
	if Verify("key", "h1", subject, r, time.Now()) == nil {
		t.Fatal("MCP entered native tool route")
	}
}
