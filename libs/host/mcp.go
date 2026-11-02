package host

import (
	"fmt"
	"net"
	"net/http"
	"time"
)

type mcpRoundTripper struct {
	client    *Client
	transport *http.Transport
}

func (t *mcpRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := t.client.perms.err("net"); err != nil {
		return nil, fmt.Errorf("invalid authorization configuration")
	}
	port := r.URL.Port()
	if port == "" {
		if r.URL.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	if !t.client.perms.netSnapshot("").Match(net.JoinHostPort(r.URL.Hostname(), port)) {
		return nil, fmt.Errorf("MCP endpoint denied by device net rules")
	}
	return t.transport.RoundTrip(r)
}

func (c *Client) mcpHTTP() *http.Client {
	return &http.Client{Transport: &mcpRoundTripper{client: c, transport: &http.Transport{IdleConnTimeout: 30 * time.Second}}, CheckRedirect: func(*http.Request, []*http.Request) error {
		return fmt.Errorf("MCP endpoint redirects are disabled; configure the final URL")
	}}
}
