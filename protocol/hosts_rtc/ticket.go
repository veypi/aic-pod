package hosts_rtc

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

	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
	"golang.org/x/crypto/hkdf"
)

const ticketDomain = "aic/host/direct-ticket/v1"
const TicketAdmissionTTL = time.Minute
const AuthorizationLease = 5 * time.Minute

// Ticket is authenticated as the exact encoded payload, not reserialized by a
// verifier. Admission expiry and connection authorization expiry are distinct.
type Ticket struct {
	Domain            string `json:"domain"`
	HostID            string `json:"host_id"`
	UserID            string `json:"user_id"`
	CredentialVersion uint64 `json:"credential_version"`
	PCID              string `json:"pc_id"`
	Fingerprint       string `json:"fingerprint"`
	ConnectionID      string `json:"connection_id,omitempty"`
	ID                string `json:"jti"`
	IssuedAt          int64  `json:"issued_at"`
	ExpiresAt         int64  `json:"expires_at"`
	LeaseUntil        int64  `json:"lease_until"`
}

func DirectKey(secret, hostID string) ([]byte, error) {
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
		return "", wire.Fail("invalid_argument", "Expected SHA-256 DTLS fingerprint")
	}
	raw := strings.ReplaceAll(p[1], ":", "")
	data, err := hex.DecodeString(raw)
	if err != nil || len(data) != sha256.Size {
		return "", wire.Fail("invalid_argument", "Invalid DTLS fingerprint")
	}
	groups := make([]string, len(data))
	for i, b := range data {
		groups[i] = fmt.Sprintf("%02X", b)
	}
	return "sha-256 " + strings.Join(groups, ":"), nil
}
func SignTicket(key []byte, ticket Ticket, now time.Time) (string, error) {
	ticket.Domain = ticketDomain
	if ticket.ID == "" {
		id := wire.NewID("t_")
		ticket.ID = id
	}
	ticket.IssuedAt = now.Unix()
	ticket.ExpiresAt = now.Add(TicketAdmissionTTL).Unix()
	ticket.LeaseUntil = now.Add(AuthorizationLease).Unix()
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
func VerifyTicket(key []byte, raw string, now time.Time) (Ticket, error) {
	var ticket Ticket
	reject := func() (Ticket, error) { return Ticket{}, wire.Fail("unauthorized", "Invalid or expired direct ticket") }
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
	if err = wire.Decode(payload, &ticket); err != nil {
		return reject()
	}
	if err = ticket.validate(now); err != nil {
		return reject()
	}
	return ticket, nil
}
func (t Ticket) validate(now time.Time) error {
	if t.Domain != ticketDomain || !wire.ValidID(t.HostID) || !wire.ValidID(t.UserID) || !wire.ValidID(t.PCID) || !wire.ValidID(t.ID) || t.CredentialVersion == 0 {
		return wire.Fail("invalid_argument", "Invalid ticket identity")
	}
	if t.ConnectionID != "" && !wire.ValidID(t.ConnectionID) {
		return wire.Fail("invalid_argument", "Invalid connection identity")
	}
	fp, err := NormalizeFingerprint(t.Fingerprint)
	if err != nil || !bytes.Equal([]byte(fp), []byte(t.Fingerprint)) {
		return wire.Fail("invalid_argument", "Invalid fingerprint")
	}
	n := now.Unix()
	if t.IssuedAt > n+5 || t.ExpiresAt <= n || t.ExpiresAt <= t.IssuedAt || t.ExpiresAt-t.IssuedAt > int64(TicketAdmissionTTL/time.Second) || t.LeaseUntil < t.ExpiresAt || t.LeaseUntil-t.IssuedAt > int64(AuthorizationLease/time.Second) {
		return wire.Fail("unauthorized", "Invalid ticket lifetime")
	}
	return nil
}
