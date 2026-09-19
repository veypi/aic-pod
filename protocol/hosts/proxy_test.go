package hosts

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestProxySignaturePurposeIdentityAndBounds(t *testing.T) {
	now := time.Now()
	key, _ := ProxyKey("secret", "host")
	direct, _ := DirectKey("secret", "host")
	if bytes.Equal(key, direct) {
		t.Fatal("proxy and RTC keys share purpose")
	}
	e := ProxyEnvelope{HostID: "host", UserID: "owner", CredentialVersion: 2, Scope: []string{"fs"}, Packet: Packet{Data: []byte(`{"v":1,"type":"request","id":"r1","method":"hello","params":{"protocol":"hosts/1"}}`)}}
	token, err := SignProxy(key, e, now)
	if err != nil {
		t.Fatal(err)
	}
	got, err := VerifyProxy(key, token, now)
	if err != nil || got.UserID != "owner" || got.Nonce == "" {
		t.Fatal(got, err)
	}
	for _, tc := range []struct {
		key   []byte
		token string
		now   time.Time
	}{{direct, token, now}, {key, token + "x", now}, {key, token, now.Add(time.Minute)}, {key, strings.Repeat("x", MaxProxyEnvelope+1), now}} {
		if _, err := VerifyProxy(tc.key, tc.token, tc.now); err == nil {
			t.Fatal("invalid proxy accepted")
		}
	}
	e.Packet.Data = []byte(`{"data":"` + strings.Repeat("x", MaxControlBytes) + `"}`)
	if _, err := SignProxy(key, e, now); err == nil {
		t.Fatal("unbounded payload signed")
	}
}
