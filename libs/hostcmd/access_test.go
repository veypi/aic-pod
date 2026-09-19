package hostcmd

import (
	"strings"
	"testing"
	"time"

	"github.com/veypi/aic-pod/protocol/hosts"
)

func TestAccessBindsTicketPeerAndLease(t *testing.T) {
	now := time.Unix(1800000000, 0)
	key, _ := hosts.DirectKey("secret", "host_1")
	a, err := NewAccess(AccessConfig{HostID: "host_1", UserID: "owner", CredentialVersion: 2, Key: key, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	fp := "sha-256 " + strings.TrimSuffix(strings.Repeat("AB:", 32), ":")
	makeTicket := func(connection string) string {
		ticket, err := hosts.SignTicket(key, hosts.Ticket{HostID: "host_1", UserID: "owner", CredentialVersion: 2, PCID: "pc_1", Fingerprint: fp, ConnectionID: connection}, now)
		if err != nil {
			t.Fatal(err)
		}
		return ticket
	}
	ticket := makeTicket("")
	if _, err = a.Admit(ticket, "another_pc", fp); err == nil {
		t.Fatal("wrong peer accepted")
	}
	admission, err := a.Admit(ticket, "pc_1", fp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.DataCaller(admission.Caller.ConnectionID); err == nil {
		t.Fatal("unbound control connection accessed data channel")
	}
	if err = a.Bind(admission.Caller.ConnectionID, "pc_2", admission.DataToken); err == nil {
		t.Fatal("cross-peer data binding")
	}
	if err = a.Bind(admission.Caller.ConnectionID, "pc_1", admission.DataToken); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DataCaller(admission.Caller.ConnectionID); err != nil {
		t.Fatal(err)
	}
	if err = a.Bind(admission.Caller.ConnectionID, "pc_1", admission.DataToken); err == nil {
		t.Fatal("data token replay")
	}
	now = now.Add(2 * time.Minute)
	renewed, err := a.Renew(admission.Caller.ConnectionID, makeTicket(admission.Caller.ConnectionID), "pc_1", fp)
	if err != nil {
		t.Fatal(err)
	}
	if !renewed.ExpiresAt.After(admission.Caller.ExpiresAt) {
		t.Fatal("lease did not renew")
	}
	a.Close(admission.Caller.ConnectionID)
	if _, err = a.Caller(admission.Caller.ConnectionID); err == nil {
		t.Fatal("closed connection remained authorized")
	}
	// Ticket replay is checked even after its original connection closes.
	fresh := makeTicket("")
	admission, err = a.Admit(fresh, "pc_1", fp)
	if err != nil {
		t.Fatal(err)
	}
	a.Close(admission.Caller.ConnectionID)
	if _, err = a.Admit(fresh, "pc_1", fp); err == nil {
		t.Fatal("ticket replay after disconnect")
	}
}

func TestProxyAccessClockSkewKeepsReplayProtection(t *testing.T) {
	issuedAt := time.Unix(1_800_000_000, 250_000_000)
	directKey, err := hosts.DirectKey("secret", "host_1")
	if err != nil {
		t.Fatal(err)
	}
	key, err := hosts.ProxyKey("secret", "host_1")
	if err != nil {
		t.Fatal(err)
	}
	for _, offset := range []time.Duration{-2 * time.Second, 2 * time.Second} {
		t.Run(offset.String(), func(t *testing.T) {
			a, err := NewAccess(AccessConfig{HostID: "host_1", UserID: "owner", CredentialVersion: 2, Key: directKey, ProxyKey: key, Now: func() time.Time { return issuedAt.Add(offset) }})
			if err != nil {
				t.Fatal(err)
			}
			envelope := hosts.ProxyEnvelope{HostID: "host_1", UserID: "owner", CredentialVersion: 2, Scope: []string{"fs"}, Packet: hosts.Packet{Data: []byte(`{"v":1,"type":"request","id":"r1","method":"hello","params":{"protocol":"hosts/1"}}`)}}
			token, err := hosts.SignProxy(key, envelope, issuedAt)
			if err != nil {
				t.Fatal(err)
			}
			_, caller, err := a.Proxy(token)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := a.Proxy(token); err == nil {
				t.Fatal("replayed proxy request accepted with clock skew")
			}
			a.Close(caller.ConnectionID)
			if _, _, err := a.Proxy(token); err == nil {
				t.Fatal("replayed proxy request accepted after disconnect with clock skew")
			}
		})
	}
}

func TestProxyAccessCannotUseRTCOrForeignAuthority(t *testing.T) {
	now := time.Now()
	key, _ := hosts.DirectKey("secret", "host_1")
	proxyKey, _ := hosts.ProxyKey("secret", "host_1")
	a, err := NewAccess(AccessConfig{HostID: "host_1", UserID: "owner", CredentialVersion: 2, Key: key, ProxyKey: proxyKey, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	envelope := hosts.ProxyEnvelope{HostID: "host_1", UserID: "owner", CredentialVersion: 2, Scope: []string{"fs"}, Packet: hosts.Packet{Data: []byte(`{"v":1,"type":"request","id":"r1","method":"hello","params":{"protocol":"hosts/1"}}`)}}
	sign := func(e hosts.ProxyEnvelope) string {
		t.Helper()
		token, err := hosts.SignProxy(proxyKey, e, now)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	for _, kind := range []string{"owner", "host", "credential"} {
		e := envelope
		switch kind {
		case "owner":
			e.UserID = "other"
		case "host":
			e.HostID = "other"
		case "credential":
			e.CredentialVersion++
		}
		if _, _, err := a.Proxy(sign(e)); err == nil {
			t.Fatalf("foreign %s accepted", kind)
		}
	}
	token := sign(envelope)
	_, caller, err := a.Proxy(token)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Proxy(token); err == nil {
		t.Fatal("replayed proxy request accepted")
	}
	if _, err := a.DataCaller(caller.ConnectionID); err != nil {
		t.Fatal(err)
	}
	a.Close(caller.ConnectionID)
	envelope.ConnectionID = caller.ConnectionID
	envelope.Packet = hosts.Packet{Data: []byte(`{"v":1,"type":"request","id":"r2","method":"auth.renew","params":{}}`)}
	if _, _, err := a.Proxy(sign(envelope)); err == nil {
		t.Fatal("closed proxy renewed")
	}
}
