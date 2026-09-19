package hosts

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRequestBoundary(t *testing.T) {
	good := `{"v":1,"type":"request","id":"c:1","method":"invoke","params":{}}`
	if _, err := ParseRequest([]byte(good)); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{good + ` {}`, strings.Replace(good, `"params":{}`, `"params":null`, 1), strings.Replace(good, `"v":1`, `"v":2`, 1), strings.Replace(good, `"type":"request"`, `"type":"request","granted_level":9`, 1), strings.Repeat(" ", MaxControlBytes) + good} {
		if _, err := ParseRequest([]byte(raw)); err == nil {
			t.Fatalf("accepted malformed request %q", raw[:min(len(raw), 150)])
		}
	}
	a, _ := CanonicalArgs(json.RawMessage(`{"z":[1,2],"a":{"b":true}}`))
	b, _ := CanonicalArgs(json.RawMessage(`{"a":{"b":true},"z":[1,2]}`))
	if string(a) != string(b) {
		t.Fatal("object order changed invocation identity")
	}
}
func TestTicketBindingAndExpiry(t *testing.T) {
	now := time.Unix(1800000000, 0)
	key, err := DirectKey("device-secret", "host_1")
	if err != nil {
		t.Fatal(err)
	}
	fp := "sha-256 " + strings.TrimSuffix(strings.Repeat("AB:", 32), ":")
	raw, err := SignTicket(key, Ticket{HostID: "host_1", UserID: "user_1", CredentialVersion: 3, PCID: "pc_1", Fingerprint: fp}, now)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := VerifyTicket(key, raw, now)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.HostID != "host_1" || decoded.CredentialVersion != 3 || decoded.Fingerprint != fp || decoded.LeaseUntil != now.Add(AuthorizationLease).Unix() {
		t.Fatalf("lost binding: %+v", decoded)
	}
	other, _ := DirectKey("device-secret", "host_2")
	for _, tc := range []struct {
		key []byte
		raw string
		at  time.Time
	}{{other, raw, now}, {key, raw + "x", now}, {key, raw, now.Add(TicketAdmissionTTL)}, {key, raw, now.Add(-10 * time.Second)}} {
		if _, err := VerifyTicket(tc.key, tc.raw, tc.at); err == nil {
			t.Fatal("accepted invalid ticket")
		}
	}
	if _, err := SignTicket(key, Ticket{HostID: "host_1", UserID: "user_1", CredentialVersion: 3, PCID: "pc_1", Fingerprint: "sha-1 AA"}, now); err == nil {
		t.Fatal("accepted weak fingerprint")
	}
}
