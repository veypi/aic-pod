package hosts

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestProxyClockSkewAndExpiry(t *testing.T) {
	issuedAt := time.Unix(1_800_000_000, 250_000_000)
	key, err := ProxyKey("secret", "host")
	if err != nil {
		t.Fatal(err)
	}
	e := ProxyEnvelope{HostID: "host", UserID: "owner", CredentialVersion: 2, Scope: []string{"fs"}, Packet: Packet{Data: []byte(`{"v":1,"type":"request","id":"r1","method":"hello","params":{"protocol":"hosts/1"}}`)}}
	token, err := SignProxy(key, e, issuedAt)
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Unix(issuedAt.Unix()+60, 0)
	for _, tc := range []struct {
		name    string
		now     time.Time
		wantErr bool
	}{
		{"same clock", issuedAt, false},
		{"device behind across second boundary", issuedAt.Add(-500 * time.Millisecond), false},
		{"device ahead across second boundary", issuedAt.Add(800 * time.Millisecond), false},
		{"device behind two seconds", issuedAt.Add(-2 * time.Second), false},
		{"device ahead two seconds", issuedAt.Add(2 * time.Second), false},
		{"future issuance at tolerance", issuedAt.Add(-5 * time.Second), false},
		{"future issuance beyond tolerance", issuedAt.Add(-6 * time.Second), true},
		{"just before expiry", expiresAt.Add(-time.Nanosecond), false},
		{"at expiry", expiresAt, true},
		{"after expiry", expiresAt.Add(time.Second), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := VerifyProxy(key, token, tc.now)
			if (err != nil) != tc.wantErr {
				t.Fatalf("VerifyProxy at %s: error = %v, want error = %v", tc.now, err, tc.wantErr)
			}
		})
	}
}

func TestProxySignaturePurposeIdentityAndBounds(t *testing.T) {
	now := time.Unix(1_800_000_000, 250_000_000)
	key, _ := ProxyKey("secret", "host")
	direct, _ := DirectKey("secret", "host")
	if bytes.Equal(key, direct) {
		t.Fatal("proxy and RTC keys share purpose")
	}
	e := ProxyEnvelope{HostID: "host", UserID: "owner", CredentialVersion: 2, Scope: []string{"fs"}, Packet: Packet{Data: []byte(`{"v":1,"type":"request","id":"r1","method":"hello","params":{"protocol":"hosts/1"}}`)}}
	// Callers cannot choose the signed admission window.
	e.IssuedAt, e.ExpiresAt = now.Add(-time.Hour).Unix(), now.Add(time.Hour).Unix()
	token, err := SignProxy(key, e, now)
	if err != nil {
		t.Fatal(err)
	}
	got, err := VerifyProxy(key, token, now)
	if err != nil || got.UserID != "owner" || got.Nonce == "" {
		t.Fatal(got, err)
	}
	if got.IssuedAt != now.Unix() || got.ExpiresAt != now.Unix()+60 {
		t.Fatalf("unexpected signed lifetime: issued_at=%d, expires_at=%d", got.IssuedAt, got.ExpiresAt)
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

func TestProxyRejectsInvalidSignedLifetime(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	key, err := ProxyKey("secret", "host")
	if err != nil {
		t.Fatal(err)
	}
	e := ProxyEnvelope{HostID: "host", UserID: "owner", CredentialVersion: 2, Scope: []string{"fs"}, Packet: Packet{Data: []byte(`{"v":1,"type":"request","id":"r1","method":"hello","params":{"protocol":"hosts/1"}}`)}}
	token, err := SignProxy(key, e, now)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func(map[string]any)
	}{
		{"missing issued_at", func(p map[string]any) { delete(p, "issued_at") }},
		{"zero issued_at", func(p map[string]any) { p["issued_at"] = 0 }},
		{"negative issued_at", func(p map[string]any) { p["issued_at"] = -1 }},
		{"missing expires_at", func(p map[string]any) { delete(p, "expires_at") }},
		{"zero lifetime", func(p map[string]any) {
			p["issued_at"], p["expires_at"] = now.Unix()+5, now.Unix()+5
		}},
		{"negative lifetime", func(p map[string]any) {
			p["issued_at"], p["expires_at"] = now.Unix()+5, now.Unix()+4
		}},
		{"lifetime exceeds sixty seconds", func(p map[string]any) { p["issued_at"] = now.Unix() - 1 }},
		{"future issuance exceeds tolerance", func(p map[string]any) { p["issued_at"] = now.Unix() + 6 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var payload map[string]any
			if err := json.Unmarshal(raw, &payload); err != nil {
				t.Fatal(err)
			}
			tc.edit(payload)
			changed, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			body := base64.RawURLEncoding.EncodeToString(changed)
			// Use a valid MAC so rejection must come from envelope validation.
			mac := hmac.New(sha256.New, key)
			mac.Write([]byte(body))
			token := body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
			if _, err := VerifyProxy(key, token, now); err == nil {
				t.Fatal("invalid signed lifetime accepted")
			}
		})
	}
}
