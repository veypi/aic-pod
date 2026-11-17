package host

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/veypi/aic-pod/protocol"
)

const (
	sessionTestUIDA    = "0123456789abcdef0123456789abcdef"
	sessionTestUIDB    = "fedcba9876543210fedcba9876543210"
	sessionTestHostIDA = sessionTestUIDA + "0001"
)

func sessionTestJWT(uid, typ string, ver uint64) string {
	payload, _ := json.Marshal(map[string]any{"uid": uid, "sub": uid, "type": typ, "ver": ver})
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." +
		base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func sessionTestOptions(uid string, provider AccessTokenProvider) Options {
	return Options{Session: &SessionConfig{
		HostID: uid + "0001", UID: uid, CredentialVer: 2, ProtocolSecret: "session-protocol-secret",
		AccessTokenProvider: provider,
	}}
}

func newSessionTestClient(t *testing.T, opts Options) *Client {
	t.Helper()
	identity, err := identityFromOptions(opts)
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{opts: opts, logf: func(string, ...any) {}}
	c.installIdentity(identity)
	return c
}

func TestSessionTokenHandlerRefreshesEveryReconnect(t *testing.T) {
	first := sessionTestJWT(sessionTestUIDA, "access", 0)
	second := sessionTestJWT(sessionTestUIDA, "access", 0)
	// 使用不同签名段模拟同账户重新登录后的新 Web key。
	second = strings.TrimSuffix(second, ".sig") + ".newsig"
	var current atomic.Value
	current.Store(first)
	var calls atomic.Int32
	provider := func(context.Context) (string, error) {
		calls.Add(1)
		return current.Load().(string), nil
	}
	c := newSessionTestClient(t, sessionTestOptions(sessionTestUIDA, provider))

	connect1, err := protocol.ParseSessionConnectToken(c.natsTokenHandler())
	if err != nil {
		t.Fatal(err)
	}
	current.Store(second)
	connect2, err := protocol.ParseSessionConnectToken(c.natsTokenHandler())
	if err != nil {
		t.Fatal(err)
	}
	if connect1.AccessToken != first || connect2.AccessToken != second {
		t.Fatalf("reconnect retained old token: first=%q second=%q", connect1.AccessToken, connect2.AccessToken)
	}
	if connect1.HostID != sessionTestHostIDA || connect2.HostID != sessionTestHostIDA || connect1.CredentialVer != 2 || connect2.CredentialVer != 2 {
		t.Fatal("token refresh changed stable Host connection profile")
	}
	if calls.Load() != 2 {
		t.Fatalf("provider calls = %d, want 2", calls.Load())
	}
}

func TestSessionHTTPUsesLatestRawBearer(t *testing.T) {
	first := sessionTestJWT(sessionTestUIDA, "access", 0)
	second := strings.TrimSuffix(first, ".sig") + ".newsig"
	current := first
	c := newSessionTestClient(t, sessionTestOptions(sessionTestUIDA, func(context.Context) (string, error) { return current, nil }))
	var got []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	cloud, err := newCloudFSWithProvider(server.URL, c.bearerToken)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		req, _ := http.NewRequest(http.MethodGet, server.URL+"/fs/cloud/u/test", nil)
		resp, err := cloud.do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		current = second
	}
	if len(got) != 2 || got[0] != "Bearer "+first || got[1] != "Bearer "+second {
		t.Fatalf("authorization headers = %q", got)
	}
	for _, header := range got {
		if strings.Contains(header, "s1.") {
			t.Fatalf("NATS envelope leaked into HTTP: %q", header)
		}
	}
}

func TestSessionProviderRejectsAccountDrift(t *testing.T) {
	c := newSessionTestClient(t, sessionTestOptions(sessionTestUIDA, func(context.Context) (string, error) {
		return sessionTestJWT(sessionTestUIDB, "access", 0), nil
	}))
	_, err := c.natsToken(context.Background())
	if !errors.Is(err, ErrSessionAccountChanged) {
		t.Fatalf("error = %v, want ErrSessionAccountChanged", err)
	}
	if token := c.natsTokenHandler(); token != "" {
		t.Fatalf("account drift returned CONNECT token %q", token)
	}
}

func TestSessionProviderFailureDoesNotReuseToken(t *testing.T) {
	var calls atomic.Int32
	c := newSessionTestClient(t, sessionTestOptions(sessionTestUIDA, func(context.Context) (string, error) {
		if calls.Add(1) == 1 {
			return sessionTestJWT(sessionTestUIDA, "access", 0), nil
		}
		return "", fmt.Errorf("refresh unavailable")
	}))
	if c.natsTokenHandler() == "" {
		t.Fatal("initial provider token missing")
	}
	if token := c.natsTokenHandler(); token != "" {
		t.Fatalf("provider failure reused stale token %q", token)
	}
}

func TestConnectionIdentityModesAreExclusive(t *testing.T) {
	provider := func(context.Context) (string, error) { return sessionTestJWT(sessionTestUIDA, "access", 0), nil }
	session := sessionTestOptions(sessionTestUIDA, provider)
	staticJWT := sessionTestJWT(sessionTestUIDA, "static", 3)
	staticOpts := Options{Key: protocol.ComposeDeviceCredential(sessionTestHostIDA, staticJWT+".secret")}
	identity, err := identityFromOptions(staticOpts)
	if err != nil {
		t.Fatal(err)
	}
	if identity.hostID != sessionTestHostIDA || identity.uid != sessionTestUIDA || identity.credVer != 3 || identity.authToken != staticJWT || identity.secret != "secret" || identity.session {
		t.Fatalf("static identity = %+v", identity)
	}
	if _, err := identityFromOptions(Options{}); err == nil {
		t.Fatal("missing auth config accepted")
	}
	mixed := staticOpts
	mixed.Session = session.Session
	if _, err := identityFromOptions(mixed); err == nil {
		t.Fatal("mixed static/session config accepted")
	}
	bad := session
	bad.Session.UID = sessionTestUIDB
	if _, err := identityFromOptions(bad); err == nil {
		t.Fatal("session uid/host mismatch accepted")
	}
	bad = sessionTestOptions(sessionTestUIDA, nil)
	if _, err := identityFromOptions(bad); err == nil {
		t.Fatal("nil token provider accepted")
	}
	bad = sessionTestOptions(sessionTestUIDA, provider)
	bad.Session.CredentialVer = 0
	if _, err := identityFromOptions(bad); err == nil {
		t.Fatal("zero generation accepted")
	}
}
