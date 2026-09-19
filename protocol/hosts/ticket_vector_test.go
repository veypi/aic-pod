package hosts

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

// Independently calculated with Python's hashlib/hmac and RFC 5869 extract /
// expand (one SHA-256 block). This catches shared signer/verifier regressions.
func TestDirectTicketFixedVector(t *testing.T) {
	key, err := DirectKey("test-secret", "host_1")
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(key); got != "a0d07f54d7265dfa39edbae58713c2662c297d47e3e4ca0788be0e24f831ca8c" {
		t.Fatalf("key vector changed: %s", got)
	}
	const expected = "eyJkb21haW4iOiJhaWMvaG9zdC9kaXJlY3QtdGlja2V0L3YxIiwiaG9zdF9pZCI6Imhvc3RfMSIsInVzZXJfaWQiOiJvd25lcl8xIiwiY3JlZGVudGlhbF92ZXJzaW9uIjoyLCJwY19pZCI6InBjXzEiLCJmaW5nZXJwcmludCI6InNoYS0yNTYgQUI6QUI6QUI6QUI6QUI6QUI6QUI6QUI6QUI6QUI6QUI6QUI6QUI6QUI6QUI6QUI6QUI6QUI6QUI6QUI6QUI6QUI6QUI6QUI6QUI6QUI6QUI6QUI6QUI6QUI6QUI6QUIiLCJqdGkiOiJ0X3ZlY3RvciIsImlzc3VlZF9hdCI6MTgwMDAwMDAwMCwiZXhwaXJlc19hdCI6MTgwMDAwMDA2MCwibGVhc2VfdW50aWwiOjE4MDAwMDAzMDB9.Il06ADg7Mji8UZDFrleDCmFIBst8KZGq6AA_XYFkfxE"
	now := time.Unix(1800000000, 0)
	ticket := Ticket{HostID: "host_1", UserID: "owner_1", CredentialVersion: 2, PCID: "pc_1", Fingerprint: "sha-256 " + strings.TrimSuffix(strings.Repeat("AB:", 32), ":"), ID: "t_vector"}
	got, err := SignTicket(key, ticket, now)
	if err != nil {
		t.Fatal(err)
	}
	if got != expected {
		t.Fatal("ticket wire vector changed")
	}
	verified, err := VerifyTicket(key, expected, now.Add(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if verified.ID != ticket.ID || verified.PCID != ticket.PCID {
		t.Fatal("vector bindings changed")
	}
}
