// Package hosts_nats binds a tool request to its authenticated destination and caller.
package hosts_nats

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	tools "github.com/veypi/aic-pod/protocol/hosts_tools"
	"strings"
	"time"
)

const Protocol = "hosts_nats/1"

type Request struct {
	HostID             string        `json:"host_id"`
	Subject            string        `json:"subject"`
	Caller             string        `json:"caller"`
	Origin             string        `json:"origin,omitempty"`
	Scope              string        `json:"scope,omitempty"`
	AuthorizationUntil int64         `json:"authorization_until_ms"`
	GrantedLevel       int           `json:"granted_level"`
	Nonce              string        `json:"nonce"`
	Deadline           int64         `json:"deadline_ms"`
	Request            tools.Request `json:"request"`
	Signature          string        `json:"signature"`
}

func Subject(uid, host string) (string, error) {
	if uid == "" || host == "" || strings.ContainsAny(uid+host, ".*> \t\n\r") {
		return "", fmt.Errorf("invalid destination")
	}
	return "u." + uid + ".h.host_" + host + ".tools.req", nil
}
func signature(key string, r Request) string {
	r.Signature = ""
	b, _ := json.Marshal(r)
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(Protocol + "\n"))
	m.Write(b)
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
func Sign(key string, r *Request) { r.Signature = signature(key, *r) }
func Verify(key, host, subject string, r Request, now time.Time) error {
	if (r.Scope != "" && r.Scope != "fs") || (r.Origin != "" && !tools.ValidID(r.Origin)) || r.AuthorizationUntil < r.Deadline || r.AuthorizationUntil > now.Add(30*time.Minute).UnixMilli() || r.Request.Protocol != Protocol || r.HostID != host || r.Subject != subject || !tools.ValidID(r.Caller) || !tools.ValidID(r.Nonce) || r.GrantedLevel < 1 || r.GrantedLevel > 9 || r.Deadline <= now.UnixMilli() || r.Deadline > now.Add(5*time.Minute).UnixMilli() {
		return tools.Fail("unauthorized", "Invalid request destination, identity or validity window")
	}
	if !hmac.Equal([]byte(signature(key, r)), []byte(r.Signature)) {
		return tools.Fail("unauthorized", "Invalid request signature")
	}
	return r.Request.Validate()
}
