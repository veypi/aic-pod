package protocol

import (
	"strings"
	"testing"
)

const (
	testSessionUID    = "0123456789abcdef0123456789abcdef"
	testSessionHostID = testSessionUID + "0007"
)

func testAccessToken(uid string) string {
	return unsignedJWT(map[string]any{"uid": uid, "sub": uid, "type": "access"})
}

func TestSessionConnectTokenRoundTrip(t *testing.T) {
	accessToken := testAccessToken(testSessionUID)
	token, err := ComposeSessionConnectToken(testSessionHostID, 3, accessToken)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseSessionConnectToken(token)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.HostID != testSessionHostID || parsed.CredentialVer != 3 || parsed.AccessToken != accessToken {
		t.Fatalf("parsed = %+v", parsed)
	}
	if got := strings.Count(token, "."); got != 5 {
		t.Fatalf("CONNECT token must preserve three JWT segments, dots = %d", got)
	}
}

func TestSessionConnectTokenRejectsInvalidInput(t *testing.T) {
	accessToken := testAccessToken(testSessionUID)
	refreshToken := unsignedJWT(map[string]any{"uid": testSessionUID, "type": "refresh"})
	staticToken := unsignedJWT(map[string]any{"uid": testSessionUID, "type": "static"})
	for name, token := range map[string]string{
		"wrong scheme":       "e1." + testSessionHostID + ".1." + accessToken,
		"short host ref":     "s1.0007.1." + accessToken,
		"old host id":        "s1." + testSessionUID + ".1." + accessToken,
		"zero host seq":      "s1." + testSessionUID + "0000.1." + accessToken,
		"zero generation":    "s1." + testSessionHostID + ".0." + accessToken,
		"bad generation":     "s1." + testSessionHostID + ".v1." + accessToken,
		"missing token":      "s1." + testSessionHostID + ".1",
		"incomplete jwt":     "s1." + testSessionHostID + ".1.header.payload",
		"refresh token":      "s1." + testSessionHostID + ".1." + refreshToken,
		"static token":       "s1." + testSessionHostID + ".1." + staticToken,
		"invalid uid":        "s1." + testSessionHostID + ".1." + testAccessToken("not-a-uuid"),
		"oversized envelope": "s1." + testSessionHostID + ".1." + accessToken + strings.Repeat("x", MaxSessionConnectTokenSize),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseSessionConnectToken(token); err == nil {
				t.Fatal("invalid token accepted")
			}
		})
	}
	if _, err := ComposeSessionConnectToken(testSessionHostID, 1, refreshToken); err == nil {
		t.Fatal("composer accepted refresh token")
	}
}

func TestValidHostID(t *testing.T) {
	for _, valid := range []string{testSessionUID + "0001", testSessionUID + "9999"} {
		if !ValidHostID(valid) {
			t.Errorf("ValidHostID(%q) = false", valid)
		}
	}
	for _, invalid := range []string{"", "0001", testSessionUID, testSessionUID + "0000", testSessionUID + "10000", strings.ToUpper(testSessionUID) + "0001", testSessionUID + "abcd"} {
		if ValidHostID(invalid) {
			t.Errorf("ValidHostID(%q) = true", invalid)
		}
	}
}
