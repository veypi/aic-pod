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
