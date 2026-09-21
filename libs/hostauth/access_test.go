package hostauth

import (
	"strings"
	"testing"
	"time"

	hosts "github.com/veypi/aic-pod/protocol/hosts_rtc"
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

