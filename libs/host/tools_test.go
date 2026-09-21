package host

import (
	"context"
	"encoding/json"
	tool "github.com/veypi/aic-pod/libs/hosts_tool"
	natswire "github.com/veypi/aic-pod/protocol/hosts_nats"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
	"testing"
	"time"
)

func TestNatsToolsAuthenticationAndSharedInstance(t *testing.T) {
	c := New(Options{Key: "host_1.1.secret.owner", WorkDir: t.TempDir(), BrowserStateDir: t.TempDir()})
	t.Cleanup(func() { _ = c.Close() })
	c.hostID, c.uid, c.kTool = "host_1", "owner", "test-tool-key"
	// Isolate admission tests from persisted device policy.
	d := tool.New(tool.Config{})
	count := 0
	if err := d.RegisterCommand(tool.DefineCommand("counter", tool.Bind(tool.Spec{Name: "next", Access: 2}, func(ctx context.Context, caller tool.Caller, _ struct{}) (int, error) { count++; return count, nil }))); err != nil {
		t.Fatal(err)
	}
	original := c.tools
	c.tools = d
	t.Cleanup(func() { _ = original.Close(context.Background()) })
	route, _ := natswire.Subject(c.uid, c.hostID)
	makeReq := func() natswire.Request {
		r := natswire.Request{HostID: c.hostID, Subject: route, Caller: c.uid, GrantedLevel: 2, Nonce: wire.NewID("n_"), Deadline: time.Now().Add(time.Minute).UnixMilli(), AuthorizationUntil: time.Now().Add(2 * time.Minute).UnixMilli(), Request: wire.Request{Protocol: natswire.Protocol, ID: wire.NewID("r_"), Action: "call", Argv: []string{"counter", "next"}}}
		natswire.Sign(c.kTool, &r)
		return r
	}
	send := func(r natswire.Request, subject string) wire.Response {
		b, _ := json.Marshal(r)
		return c.HandleNATS(context.Background(), subject, b)
	}
	r := makeReq()
	if got := send(r, route); got.Error != nil || got.Result != 1 {
		t.Fatalf("%+v", got)
	}
	if got := send(r, route); got.Error == nil {
		t.Fatal("replay admitted")
	}
	r = makeReq()
	r.Request.Action = "stream.open"
	natswire.Sign(c.kTool, &r)
	if got := send(r, route); got.Error == nil {
		t.Fatal("NATS exposed RTC-only stream")
	}
	r = makeReq()
	r.GrantedLevel = 9
	if got := send(r, route); got.Error == nil {
		t.Fatal("tampered grant admitted")
	}
	if got := send(makeReq(), route+".bad"); got.Error == nil {
		t.Fatal("wrong route admitted")
	}
	r = makeReq()
	r.Caller = "foreign"
	natswire.Sign(c.kTool, &r)
	if got := send(r, route); got.Error == nil {
		t.Fatal("foreign owner admitted")
	}
	got := c.HandleTool(context.Background(), tool.Caller{Subject: c.uid, ConnectionID: "rtc", Level: 2, ExpiresAt: time.Now().Add(time.Minute)}, wire.Request{Protocol: "hosts_rtc/1", ID: "r_rtc", Action: "call", Argv: []string{"counter", "next"}})
	if got.Error != nil || got.Result != 2 {
		t.Fatalf("transports didn't share a tool: %+v", got)
	}
}
