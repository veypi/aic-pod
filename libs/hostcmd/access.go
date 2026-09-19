package hostcmd

import (
	"crypto/sha256"
	"crypto/subtle"
	"sync"
	"time"

	"github.com/veypi/aic-pod/protocol/hosts"
)

type AccessConfig struct {
	HostID            string
	UserID            string
	CredentialVersion uint64
	Key               []byte
	ProxyKey          []byte
	Now               func() time.Time
	MaxConnections    int
}
type Admission struct {
	Caller    Caller
	DataToken string
}
type connection struct {
	caller            Caller
	pcID, fingerprint string
	token             [32]byte
	bindUntil         time.Time
	bound             bool
	proxy             bool
}

// Access binds signed tickets to the DTLS peer observed by the transport. The
// remote fingerprint argument must come from RTC, never from a hello parameter.
type Access struct {
	mu          sync.Mutex
	cfg         AccessConfig
	consumed    map[string]time.Time
	nextSweep   time.Time
	connections map[string]*connection
}

func NewAccess(cfg AccessConfig) (*Access, error) {
	if !hosts.ValidID(cfg.HostID) || !hosts.ValidID(cfg.UserID) || cfg.CredentialVersion == 0 || len(cfg.Key) != 32 {
		return nil, hosts.Fail("invalid_argument", "Invalid device authorization configuration")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MaxConnections <= 0 {
		cfg.MaxConnections = 16
	}
	cfg.Key = append([]byte(nil), cfg.Key...)
	cfg.ProxyKey = append([]byte(nil), cfg.ProxyKey...)
	return &Access{cfg: cfg, consumed: map[string]time.Time{}, connections: map[string]*connection{}}, nil
}
func (a *Access) check(ticket, pcID, fingerprint string) (hosts.Ticket, error) {
	t, err := hosts.VerifyTicket(a.cfg.Key, ticket, a.cfg.Now())
	if err != nil {
		return t, err
	}
	fp, err := hosts.NormalizeFingerprint(fingerprint)
	if err != nil || t.HostID != a.cfg.HostID || t.UserID != a.cfg.UserID || t.CredentialVersion != a.cfg.CredentialVersion || t.PCID != pcID || t.Fingerprint != fp {
		return hosts.Ticket{}, hosts.Fail("unauthorized", "Direct ticket does not match this DTLS connection")
	}
	return t, nil
}
func (a *Access) consume(t hosts.Ticket) error {
	now := a.cfg.Now()
	if !now.Before(a.nextSweep) {
		a.nextSweep = now.Add(time.Second)
		for id, until := range a.consumed {
			if !until.After(now) {
				delete(a.consumed, id)
			}
		}
	}
	if _, exists := a.consumed[t.ID]; exists {
		return hosts.Fail("unauthorized", "Direct ticket has already been used")
	}
	if len(a.consumed) >= 32768 {
		return hosts.Fail("overloaded", "Ticket admission quota reached")
	}
	a.consumed[t.ID] = time.Unix(t.ExpiresAt, 0)
	return nil
}
func (a *Access) Admit(ticket, pcID, remoteFingerprint string) (Admission, error) {
	t, err := a.check(ticket, pcID, remoteFingerprint)
	if err != nil {
		return Admission{}, err
	}
	if t.ConnectionID != "" {
		return Admission{}, hosts.Fail("unauthorized", "A renewal ticket cannot open a connection")
	}
	id, err := hosts.NewID("conn_")
	if err != nil {
		return Admission{}, err
	}
	token, err := hosts.NewID("bind_")
	if err != nil {
		return Admission{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, existing := range a.connections {
		if existing.pcID == pcID {
			return Admission{}, hosts.Fail("unauthorized", "Peer connection is already authenticated")
		}
	}
	if len(a.connections) >= a.cfg.MaxConnections {
		return Admission{}, hosts.Fail("overloaded", "Connection limit reached")
	}
	if err = a.consume(t); err != nil {
		return Admission{}, err
	}
	caller := Caller{Subject: t.UserID, ConnectionID: id, Transport: "rtc", ExpiresAt: time.Unix(t.LeaseUntil, 0)}
	a.connections[id] = &connection{caller: caller, pcID: pcID, fingerprint: t.Fingerprint, token: sha256.Sum256([]byte(token)), bindUntil: a.cfg.Now().Add(5 * time.Second)}
	return Admission{Caller: caller, DataToken: token}, nil
}
func (a *Access) Bind(connectionID, pcID, token string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.connections[connectionID]
	hash := sha256.Sum256([]byte(token))
	if c == nil || c.pcID != pcID || c.bound || !c.bindUntil.After(a.cfg.Now()) || !c.caller.ExpiresAt.After(a.cfg.Now()) || subtle.ConstantTimeCompare(hash[:], c.token[:]) != 1 {
		return hosts.Fail("unauthorized", "Invalid or expired data-channel binding")
	}
	c.bound = true
	return nil
}
func (a *Access) Caller(connectionID string) (Caller, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.connections[connectionID]
	if c == nil || !c.caller.ExpiresAt.After(a.cfg.Now()) {
		return Caller{}, hosts.Fail("unauthorized", "Connection authorization expired")
	}
	return c.caller, nil
}

// DataCaller additionally requires the one-use data-channel binding. A control
// channel connection ID alone is never sufficient for byte access.
func (a *Access) DataCaller(connectionID string) (Caller, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.connections[connectionID]
	if c == nil || !c.bound || !c.caller.ExpiresAt.After(a.cfg.Now()) {
		return Caller{}, hosts.Fail("unauthorized", "Data channel is not authorized")
	}
	return c.caller, nil
}
func (a *Access) Renew(connectionID, ticket, pcID, remoteFingerprint string) (Caller, error) {
	t, err := a.check(ticket, pcID, remoteFingerprint)
	if err != nil {
		return Caller{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.connections[connectionID]
	if c == nil || !c.caller.ExpiresAt.After(a.cfg.Now()) || t.ConnectionID != connectionID || c.pcID != pcID || c.fingerprint != t.Fingerprint {
		return Caller{}, hosts.Fail("unauthorized", "Renewal does not match the active connection")
	}
	if err = a.consume(t); err != nil {
		return Caller{}, err
	}
	until := time.Unix(t.LeaseUntil, 0)
	if until.After(c.caller.ExpiresAt) {
		c.caller.ExpiresAt = until
	}
	return c.caller, nil
}
func (a *Access) Close(connectionID string) {
	a.mu.Lock()
	delete(a.connections, connectionID)
	a.mu.Unlock()
}
func (a *Access) RevokeAll() { a.mu.Lock(); a.connections = map[string]*connection{}; a.mu.Unlock() }

// Proxy verifies each server-forwarded request. Only this signed admission may
// create/renew proxy callers; RTC callers still require observed DTLS binding.
func (a *Access) Proxy(token string) (hosts.ProxyEnvelope, Caller, error) {
	e, err := hosts.VerifyProxy(a.cfg.ProxyKey, token, a.cfg.Now())
	if err != nil {
		return e, Caller{}, err
	}
	if e.HostID != a.cfg.HostID || e.UserID != a.cfg.UserID || e.CredentialVersion != a.cfg.CredentialVersion {
		return e, Caller{}, hosts.Fail("unauthorized", "Proxy identity mismatch")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.consume(hosts.Ticket{ID: e.Nonce, ExpiresAt: e.ExpiresAt}); err != nil {
		return e, Caller{}, err
	}
	id := e.ConnectionID
	_, method := e.Packet.Identity()
	if method == "hello" {
		if id != "" {
			return e, Caller{}, hosts.Fail("invalid_argument", "hello cannot select a connection")
		}
		if len(a.connections) >= a.cfg.MaxConnections {
			return e, Caller{}, hosts.Fail("overloaded", "Connection limit reached")
		}
		id, err = hosts.NewID("proxy_")
		if err != nil {
			return e, Caller{}, err
		}
		a.connections[id] = &connection{caller: Caller{Subject: e.UserID, ConnectionID: id, Scopes: []string{"fs"}, Transport: "proxy"}, bound: true, proxy: true}
	}
	c := a.connections[id]
	if c == nil || !c.proxy || (method != "hello" && !c.caller.ExpiresAt.After(a.cfg.Now())) {
		return e, Caller{}, hosts.Fail("unauthorized", "Proxy connection expired")
	}
	c.caller.ExpiresAt = a.cfg.Now().Add(hosts.AuthorizationLease)
	return e, c.caller, nil
}

func (a *Access) Expired() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var ids []string
	for id, c := range a.connections {
		if !c.caller.ExpiresAt.After(a.cfg.Now()) {
			delete(a.connections, id)
			ids = append(ids, id)
		}
	}
	return ids
}
