package host

import (
	"context"
	"fmt"
	"github.com/veypi/aic-pod/protocol"
	"net"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/execution"
	"github.com/veypi/aic-pod/libs/mcpx"

	"github.com/veypi/vbox"
)

func (c *Client) configureMCP(settings mcpx.Settings) error {
	servers, err := mcpServers(settings.Servers, c.options().WorkDir)
	if err != nil {
		return err
	}
	manager, err := mcpx.NewManager(servers, mcpx.Options{Authorize: c.authorizeMCP, HTTPClient: c.mcpHTTP(), Processes: c.procs, Logf: c.logf, Policy: func(cfg mcpx.Config) vbox.Policy {
		return c.nativePolicy(context.Background(), cfg.Cwd, cfg.Command)
	}})
	if err != nil {
		return err
	}
	c.mcpMu.Lock()
	if c.browserCancel != nil {
		c.browserCancel()
	}
	if c.mcpServices != nil {
		c.mcpServices.Close()
		if c.mcpServices.Started("browser") {
			c.closeBrowser(c.browserConfig)
		}
	}
	c.browserConfig = nil
	if _, overridden := settings.Servers["browser"]; !overridden {
		browser := servers["browser"]
		c.browserConfig = &browser
	}
	c.browserContext, c.browserCancel = context.WithCancel(context.Background())
	c.mcpServices = manager
	c.mcpAuth = cfg.AuthSnapshot()
	c.mcpMu.Unlock()
	return nil
}

func (c *Client) mcpSession(ctx context.Context, server string) (*mcp.ClientSession, error) {
	if !c.execAllowed(execution.SessionFromContext(ctx), "mcp."+server) {
		return nil, fmt.Errorf("mcp.%s: denied by exec rules; request grant cmd mcp.%s", server, server)
	}
	c.mcpMu.RLock()
	manager := c.mcpServices
	c.mcpMu.RUnlock()
	if manager == nil {
		return nil, fmt.Errorf("MCP unavailable")
	}
	return manager.Session(ctx, server)
}

// MCP service access is authorized at the command boundary. Tool schemas and
// service-specific permission checks belong to the upstream server.
func (c *Client) authorizeMCP(ctx context.Context, server, _ string, _ mcp.Params) error {
	if !c.execAllowed(execution.SessionFromContext(ctx), "mcp."+server) {
		return protocol.Fail("permission_denied", "Service denied; request grant cmd mcp."+server)
	}
	return nil
}

type mcpRoundTripper struct {
	client    *Client
	transport *http.Transport
}

func (t *mcpRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	if cfg.CheckAuth() != nil {
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
	if !t.client.netPol.Snapshot("").Match(net.JoinHostPort(r.URL.Hostname(), port)) {
		return nil, fmt.Errorf("MCP endpoint denied by device net rules")
	}
	return t.transport.RoundTrip(r)
}
func (c *Client) mcpHTTP() *http.Client {
	return &http.Client{Transport: &mcpRoundTripper{client: c, transport: &http.Transport{IdleConnTimeout: 30 * time.Second}}, CheckRedirect: func(*http.Request, []*http.Request) error {
		return fmt.Errorf("MCP endpoint redirects are disabled; configure the final URL")
	}}
}
