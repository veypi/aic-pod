package protocol

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"golang.org/x/crypto/hkdf"
)

const ticketDomain = "aic/host/direct-ticket/v1"
const RtcTicketAdmissionTTL = time.Minute
const RtcAuthorizationLease = 5 * time.Minute

// RtcTicket is authenticated as the exact encoded payload, not reserialized by a
// verifier. Admission expiry and connection authorization expiry are distinct.
type RtcTicket struct {
	Domain            string `json:"domain"`
	HostID            string `json:"host_id"`
	UserID            string `json:"user_id"`
	CredentialVersion uint64 `json:"credential_version"`
	PCID              string `json:"pc_id"`
	Fingerprint       string `json:"fingerprint"`
	ConnectionID      string `json:"connection_id,omitempty"`
	// SessionID 绑定 RTC 请求归属会话（hosts-vsh-redesign §2.4：RTC 的
	// session_id 必须由票据绑定，不能由脚本或未经核验的请求字段冒认）。
	SessionID  string `json:"session_id,omitempty"`
	ID         string `json:"jti"`
	IssuedAt   int64  `json:"issued_at"`
	ExpiresAt  int64  `json:"expires_at"`
	LeaseUntil int64  `json:"lease_until"`
}

func RtcDirectKey(secret, hostID string) ([]byte, error) {
	if secret == "" || hostID == "" {
		return nil, fmt.Errorf("missing direct key material")
	}
	key := make([]byte, 32)
	_, err := io.ReadFull(hkdf.New(sha256.New, []byte(secret), []byte(hostID), []byte(ticketDomain)), key)
	return key, err
}
func NormalizeFingerprint(s string) (string, error) {
	p := strings.Fields(s)
	if len(p) != 2 || strings.ToLower(p[0]) != "sha-256" {
		return "", Fail("invalid_argument", "Expected SHA-256 DTLS fingerprint")
	}
	raw := strings.ReplaceAll(p[1], ":", "")
	data, err := hex.DecodeString(raw)
	if err != nil || len(data) != sha256.Size {
		return "", Fail("invalid_argument", "Invalid DTLS fingerprint")
	}
	groups := make([]string, len(data))
	for i, b := range data {
		groups[i] = fmt.Sprintf("%02X", b)
	}
	return "sha-256 " + strings.Join(groups, ":"), nil
}
func SignRtcTicket(key []byte, ticket RtcTicket, now time.Time) (string, error) {
	ticket.Domain = ticketDomain
	if ticket.ID == "" {
		id := NewID("t_")
		ticket.ID = id
	}
	ticket.IssuedAt = now.Unix()
	ticket.ExpiresAt = now.Add(RtcTicketAdmissionTTL).Unix()
	ticket.LeaseUntil = now.Add(RtcAuthorizationLease).Unix()
	fp, err := NormalizeFingerprint(ticket.Fingerprint)
	if err != nil {
		return "", err
	}
	ticket.Fingerprint = fp
	if err := ticket.validate(now); err != nil {
		return "", err
	}
	if len(key) != 32 {
		return "", fmt.Errorf("invalid direct key")
	}
	payload, err := json.Marshal(ticket)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}
func VerifyRtcTicket(key []byte, raw string, now time.Time) (RtcTicket, error) {
	var ticket RtcTicket
	reject := func() (RtcTicket, error) {
		return RtcTicket{}, Fail("unauthorized", "Invalid or expired direct ticket")
	}
	if len(key) != 32 || len(raw) > 4096 {
		return reject()
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 2 {
		return reject()
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return reject()
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(parts[0]))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return reject()
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return reject()
	}
	if err = Decode(payload, &ticket); err != nil {
		return reject()
	}
	if err = ticket.validate(now); err != nil {
		return reject()
	}
	return ticket, nil
}
func (t RtcTicket) validate(now time.Time) error {
	if t.Domain != ticketDomain || !ValidID(t.HostID) || !ValidID(t.UserID) || !ValidID(t.PCID) || !ValidID(t.ID) || t.CredentialVersion == 0 {
		return Fail("invalid_argument", "Invalid ticket identity")
	}
	if t.ConnectionID != "" && !ValidID(t.ConnectionID) {
		return Fail("invalid_argument", "Invalid connection identity")
	}
	if t.SessionID != "" && !ValidID(t.SessionID) {
		return Fail("invalid_argument", "Invalid session identity")
	}
	fp, err := NormalizeFingerprint(t.Fingerprint)
	if err != nil || !bytes.Equal([]byte(fp), []byte(t.Fingerprint)) {
		return Fail("invalid_argument", "Invalid fingerprint")
	}
	n := now.Unix()
	if t.IssuedAt > n+5 || t.ExpiresAt <= n || t.ExpiresAt <= t.IssuedAt || t.ExpiresAt-t.IssuedAt > int64(RtcTicketAdmissionTTL/time.Second) || t.LeaseUntil < t.ExpiresAt || t.LeaseUntil-t.IssuedAt > int64(RtcAuthorizationLease/time.Second) {
		return Fail("unauthorized", "Invalid ticket lifetime")
	}
	return nil
}
