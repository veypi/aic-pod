package host

import (
	"context"
	"encoding/json"
	"github.com/veypi/aic-pod/protocol"
	"testing"
	"time"
)

// TestNatsToolsAuthentication 锁定 NATS 可信转发边界（hosts_nats/2）：
// 验签（含 grant_approved 防篡改）、路由、nonce 去重、归属；RTC 与 NATS
// 同一分发实现。
func TestNatsToolsAuthentication(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := New(Options{Key: "host_1.1.secret.owner", WorkDir: t.TempDir()})
	t.Cleanup(func() { _ = c.Close() })
	c.hostID, c.uid, c.kTool = "host_1", "owner", "test-tool-key"
	route, _ := protocol.NatsSubject(c.uid, c.hostID)
	makeReq := func() protocol.NatsRequest {
		r := protocol.NatsRequest{HostID: c.hostID, Subject: route, Caller: c.uid, Nonce: protocol.NewID("n_"), Deadline: time.Now().Add(time.Minute).UnixMilli(), AuthorizationUntil: time.Now().Add(2 * time.Minute).UnixMilli(), Request: fsRequest("roots", map[string]any{})}
		protocol.NatsSign(c.kTool, &r)
		return r
	}
	send := func(r protocol.NatsRequest, subject string) protocol.Response {
		b, _ := json.Marshal(r)
		return callNATS(t, c, subject, b)
	}
	r := makeReq()
	if got := send(r, route); got.Error != nil {
		t.Fatalf("%+v", got)
	}
	if got := send(r, route); got.Error == nil {
		t.Fatal("replay admitted")
	}
	r = makeReq()
	r.Request.Action = "stream.open"
	protocol.NatsSign(c.kTool, &r)
	if got := send(r, route); got.Error == nil {
		t.Fatal("NATS exposed RTC-only stream")
	}
	r = makeReq()
	r.GrantApproved = true // 篡改签名后字段 → 验签失败
	if got := send(r, route); got.Error == nil {
		t.Fatal("tampered grant_approved admitted")
	}
	if got := send(makeReq(), route+".bad"); got.Error == nil {
		t.Fatal("wrong route admitted")
	}
	r = makeReq()
	r.Caller = "foreign"
	protocol.NatsSign(c.kTool, &r)
	if got := send(r, route); got.Error == nil {
		t.Fatal("foreign owner admitted")
	}
	// RTC 与 NATS 同一分发实现（普通请求同载荷同语义）。
	got := callTool(t, c, context.Background(), testCaller(), fsRequest("roots", map[string]any{}))
	if got.Error != nil {
		t.Fatalf("transports didn't share dispatch: %+v", got)
	}
}
