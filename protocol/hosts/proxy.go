package hosts

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"time"

	"golang.org/x/crypto/hkdf"
)

const proxyDomain = "aic/host/owner-proxy/v1"
const MaxProxyEnvelope = 192 << 10

// Packet is only a transport adapter container. HTTP exposes Data directly;
// the signed NATS envelope covers these exact bytes and their content type.
type Packet struct {
	Binary bool   `json:"binary,omitempty"`
	Data   []byte `json:"data"`
}

func JSONPacket(value any) Packet { raw, _ := json.Marshal(value); return Packet{Data: raw} }
func (p Packet) Identity() (string, string) {
	if p.Binary {
		h, _, _ := DecodeFrame(p.Data)
		return h.RequestID, "bytes.chunk"
	}
	r, _ := ParseRequest(p.Data)
	return r.ID, r.Method
}

// ValidateProxy is an explicit positive allow-list. Indirect references are
// additionally restricted by the immutable filesystem session scope on device.
func (p Packet) ValidateProxy() error {
	if p.Binary {
		h, data, err := DecodeFrame(p.Data)
		if err != nil {
			return err
		}
		if !ValidID(h.RequestID) || !ValidID(h.SessionID) || len(data) > ChunkBytes {
			return Fail("invalid_argument", "Invalid file chunk")
		}
		return nil
	}
	r, err := ParseRequest(p.Data)
	if err != nil {
		return err
	}
	switch r.Method {
	case "hello", "auth.renew", "connection.close", "session.open", "session.resume", "session.close", "catalog.get", "operation.get", "operation.cancel", "bytes.create", "bytes.read", "bytes.seal", "stream.pull", "stream.status", "stream.cancel", "resource.describe", "resource.release":
		return nil
	case "invoke":
		var p struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(r.Params, &p) != nil {
			return Fail("invalid_argument", "Invalid invocation")
		}
		if p.Command == "fs" {
			return nil
		}
	}
	return Fail("unsupported", "Proxy currently supports filesystem commands only")
}

type ProxyEnvelope struct {
	Domain            string   `json:"domain"`
	HostID            string   `json:"host_id"`
	UserID            string   `json:"user_id"`
	CredentialVersion uint64   `json:"credential_version"`
	ConnectionID      string   `json:"connection_id,omitempty"`
	Scope             []string `json:"scope"`
	Nonce             string   `json:"nonce"`
	ExpiresAt         int64    `json:"expires_at"`
	Packet            Packet   `json:"packet"`
}

func (e ProxyEnvelope) validate() error {
	if e.ConnectionID != "" && !ValidID(e.ConnectionID) {
		return Fail("invalid_argument", "Invalid proxy connection")
	}
	if len(e.Scope) != 1 || e.Scope[0] != "fs" {
		return Fail("permission_denied", "Proxy scope must be fs")
	}
	return e.Packet.ValidateProxy()
}

func ProxyKey(secret, hostID string) ([]byte, error) {
	if secret == "" || hostID == "" {
		return nil, Fail("unauthorized", "Missing proxy key material")
	}
	key := make([]byte, 32)
	_, err := io.ReadFull(hkdf.New(sha256.New, []byte(secret), []byte(hostID), []byte(proxyDomain)), key)
	return key, err
}
func SignProxy(key []byte, e ProxyEnvelope, now time.Time) (string, error) {
	if err := e.validate(); err != nil {
		return "", err
	}
	e.Domain = proxyDomain
	var err error
	e.Nonce, err = NewID("proxy_")
	if err != nil {
		return "", err
	}
	e.ExpiresAt = now.Add(time.Minute).Unix()
	if len(key) != 32 || !ValidID(e.HostID) || !ValidID(e.UserID) || e.CredentialVersion == 0 {
		return "", Fail("unauthorized", "Invalid proxy identity")
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	body := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(body))
	token := body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if len(token) > MaxProxyEnvelope {
		return "", Fail("invalid_argument", "Proxy request too large")
	}
	return token, nil
}
func VerifyProxy(key []byte, token string, now time.Time) (ProxyEnvelope, error) {
	reject := func() (ProxyEnvelope, error) {
		return ProxyEnvelope{}, Fail("unauthorized", "Invalid or expired owner proxy request")
	}
	if len(key) != 32 || len(token) > MaxProxyEnvelope {
		return reject()
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return reject()
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(parts[0]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || !hmac.Equal(sig, mac.Sum(nil)) {
		return reject()
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return reject()
	}
	var e ProxyEnvelope
	if json.Unmarshal(raw, &e) != nil || e.Domain != proxyDomain || !ValidID(e.Nonce) || !ValidID(e.HostID) || !ValidID(e.UserID) || e.CredentialVersion == 0 || e.ExpiresAt <= now.Unix() || e.ExpiresAt > now.Add(time.Minute).Unix() || e.validate() != nil {
		return reject()
	}
	return e, nil
}
