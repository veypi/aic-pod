package vsh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/veypi/vbox"
	vshnet "github.com/veypi/vsh/network"
)

// fakeResolver 把任意主机名解析为公网 IP（绕过私网阻断，专注测规则表/审计）。
type fakeResolver struct{}

func (fakeResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
}

// hijackDial 把所有连接导向测试 server（公网语义的本地替身）。
func hijackDial(server *httptest.Server) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", server.Listener.Addr().String())
	}
}

func TestNetClientPrivateBlocked(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "should-not-reach")
	}))
	defer server.Close()
	var audits []NetAudit
	c := NewNetClient(NetClientConfig{
		Audit: func(a NetAudit) { audits = append(audits, a) },
	})
	// httptest 监听 127.0.0.1——loopback 必须被阻断（验收 5）。
	_, err := c.Do(context.Background(), &vshnet.Request{Method: "GET", URL: server.URL})
	var denied *vshnet.AccessDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("loopback should be blocked: %v", err)
	}
	// 审计字段齐全：Blocked + Reason + 耗时。
	if len(audits) != 1 || !audits[0].Blocked || audits[0].Reason == "" {
		t.Fatalf("audit = %+v", audits)
	}
	// 链路本地（云 metadata）显式阻断。
	_, err = c.Do(context.Background(), &vshnet.Request{Method: "GET", URL: "http://169.254.169.254/latest/meta-data"})
	if !errors.As(err, &denied) {
		t.Fatalf("link-local should be blocked: %v", err)
	}
}

func TestNetClientRuleTableDeny(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	}))
	defer server.Close()
	rules := vbox.NetRuleSet{
		Rules:   []vbox.NetRule{{HostPort: "example.com:80", Allow: false}},
		Default: true, // net_policy open
	}
	c := NewNetClient(NetClientConfig{
		Rules:       func() vbox.NetRuleSet { return rules },
		Resolver:    fakeResolver{},
		DialContext: hijackDial(server),
	})
	// deny 行命中 → 拒且引导 grant。
	_, err := c.Do(context.Background(), &vshnet.Request{Method: "GET", URL: "http://example.com/x"})
	if err == nil || !strings.Contains(err.Error(), "grant net") {
		t.Fatalf("deny rule = %v", err)
	}
	// 未命中 → default open 放行。
	resp, err := c.Do(context.Background(), &vshnet.Request{Method: "GET", URL: "http://other.com/x"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || strings.TrimSpace(string(resp.Body)) != "ok" {
		t.Fatalf("resp = %d %q", resp.StatusCode, resp.Body)
	}
}

func TestNetClientRedirectRevalidated(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/jump" {
			http.Redirect(w, r, "http://127.0.0.1:1/evil", http.StatusFound)
			return
		}
		fmt.Fprintln(w, "ok")
	}))
	defer server.Close()
	var mu sync.Mutex
	var audits []NetAudit
	c := NewNetClient(NetClientConfig{
		Audit:       func(a NetAudit) { mu.Lock(); audits = append(audits, a); mu.Unlock() },
		Resolver:    fakeResolver{},
		DialContext: hijackDial(server),
	})
	// 首跳放行（公网伪装），重定向目标 loopback → 逐跳复核拦截。
	_, err := c.Do(context.Background(), &vshnet.Request{Method: "GET", URL: "http://example.com/jump", FollowRedirects: true})
	var denied *vshnet.AccessDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("redirect to loopback should be blocked: %v", err)
	}
}

func TestNetClientResponseCap(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("x", 4096)))
	}))
	defer server.Close()
	c := NewNetClient(NetClientConfig{
		MaxResponseBytes: 1024,
		Resolver:         fakeResolver{},
		DialContext:      hijackDial(server),
	})
	_, err := c.Do(context.Background(), &vshnet.Request{Method: "GET", URL: "http://example.com/big"})
	var tooLarge *vshnet.ResponseTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("oversize = %v", err)
	}
}

func TestNetClientQuotaPrecheck(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "2048")
		w.Write(make([]byte, 2048))
	}))
	defer server.Close()
	quotaErr := errors.New("storage quota exceeded")
	c := NewNetClient(NetClientConfig{
		OnResponseSize: func(size int64) error {
			if size > 1024 {
				return quotaErr
			}
			return nil
		},
		Resolver:    fakeResolver{},
		DialContext: hijackDial(server),
	})
	_, err := c.Do(context.Background(), &vshnet.Request{Method: "GET", URL: "http://example.com/file"})
	if !errors.Is(err, quotaErr) {
		t.Fatalf("quota precheck = %v", err)
	}
}

func TestNetClientAuditFields(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "audited")
	}))
	defer server.Close()
	var audits []NetAudit
	c := NewNetClient(NetClientConfig{
		Audit:       func(a NetAudit) { audits = append(audits, a) },
		Resolver:    fakeResolver{},
		DialContext: hijackDial(server),
	})
	_, err := c.Do(context.Background(), &vshnet.Request{Method: "GET", URL: "http://example.com/ok"})
	if err != nil {
		t.Fatal(err)
	}
	if len(audits) != 1 {
		t.Fatalf("audits = %v", audits)
	}
	a := audits[0]
	if a.Status != 200 || a.Bytes == 0 || a.Blocked || a.URL != "http://example.com/ok" {
		t.Fatalf("audit fields incomplete: %+v", a)
	}
}
