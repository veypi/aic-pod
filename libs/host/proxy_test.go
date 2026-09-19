package host

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/veypi/aic-pod/protocol/hosts"
)

func TestProxyCommandsRunWithoutRTC(t *testing.T) {
	c := New(Options{Key: "host_1.1.secret.owner", WorkDir: t.TempDir(), RTC: false})
	if err := c.startCommands(); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	m := c.buildMgmt()
	if m == nil || m.Transports["rtc"].Enabled || !m.Transports["proxy"].Supports(hosts.Protocol, "fs") {
		t.Fatalf("incorrect RTC-off capability: %+v", m)
	}
	key, _ := hosts.ProxyKey("secret", "host_1")
	e := hosts.ProxyEnvelope{HostID: "host_1", UserID: "owner", CredentialVersion: 1, Scope: []string{"fs"}, Packet: hosts.Packet{Data: []byte(`{"v":1,"type":"request","id":"r1","method":"hello","params":{"protocol":"hosts/1"}}`)}}
	token, err := hosts.SignProxy(key, e, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	decode := func(p hosts.Packet) hosts.Response {
		var r hosts.Response
		if err := json.Unmarshal(p.Data, &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := decode(c.commands.HandleProxy(context.Background(), token))
	if !r.OK {
		t.Fatal(r.Error)
	}
	if r = decode(c.commands.HandleProxy(context.Background(), token)); r.OK || r.Error.Code != "unauthorized" {
		t.Fatal("replayed request accepted", r)
	}
	if r = decode(c.commands.HandleProxy(context.Background(), `{"tool":"fs","granted_level":9}`)); r.OK {
		t.Fatal("unsigned/AI request accepted as owner")
	}
}
