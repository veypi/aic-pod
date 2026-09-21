package hostauth

import (
	"sync"
	"time"

	hosts "github.com/veypi/aic-pod/protocol/hosts_rtc"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
)

type AccessConfig struct {
	HostID            string
	UserID            string
	CredentialVersion uint64
	Key               []byte
	Now               func() time.Time
	MaxConnections    int
}
type Admission struct {
	Caller Caller
}
type connection struct {
	caller            Caller
	pcID, fingerprint string
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
	if !wire.ValidID(cfg.HostID) || !wire.ValidID(cfg.UserID) || cfg.CredentialVersion == 0 || len(cfg.Key) != 32 {
		return nil, wire.Fail("invalid_argument", "Invalid device authorization configuration")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MaxConnections <= 0 {
		cfg.MaxConnections = 16
	}
	cfg.Key = append([]byte(nil), cfg.Key...)
	return &Access{cfg: cfg, consumed: map[string]time.Time{}, connections: map[string]*connection{}}, nil
}
func (a *Access) check(ticket, pcID, fingerprint string) (hosts.Ticket, error) {
	t, err := hosts.VerifyTicket(a.cfg.Key, ticket, a.cfg.Now())
	if err != nil {
		return t, err
	}
	fp, err := hosts.NormalizeFingerprint(fingerprint)
	if err != nil || t.HostID != a.cfg.HostID || t.UserID != a.cfg.UserID || t.CredentialVersion != a.cfg.CredentialVersion || t.PCID != pcID || t.Fingerprint != fp {
		return hosts.Ticket{}, wire.Fail("unauthorized", "Direct ticket does not match this DTLS connection")
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
		return wire.Fail("unauthorized", "Direct ticket has already been used")
	}
	if len(a.consumed) >= 32768 {
		return wire.Fail("overloaded", "Ticket admission quota reached")
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
		return Admission{}, wire.Fail("unauthorized", "A renewal ticket cannot open a connection")
	}
	id := wire.NewID("conn_")
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, existing := range a.connections {
		if existing.pcID == pcID {
			return Admission{}, wire.Fail("unauthorized", "Peer connection is already authenticated")
		}
	}
	if len(a.connections) >= a.cfg.MaxConnections {
		return Admission{}, wire.Fail("overloaded", "Connection limit reached")
	}
	if err = a.consume(t); err != nil {
		return Admission{}, err
	}
	caller := Caller{Subject: t.UserID, ConnectionID: id, Transport: "rtc", ExpiresAt: time.Unix(t.LeaseUntil, 0)}
	a.connections[id] = &connection{caller: caller, pcID: pcID, fingerprint: t.Fingerprint}
	return Admission{Caller: caller}, nil
}
func (a *Access) Caller(connectionID string) (Caller, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.connections[connectionID]
	if c == nil || !c.caller.ExpiresAt.After(a.cfg.Now()) {
		return Caller{}, wire.Fail("unauthorized", "Connection authorization expired")
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
		return Caller{}, wire.Fail("unauthorized", "Renewal does not match the active connection")
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

type Caller struct {
	Subject, ConnectionID, Transport string
	ExpiresAt                        time.Time
}
