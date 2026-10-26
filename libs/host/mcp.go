package host

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/veypi/aic-pod/libs/execution"
	"github.com/veypi/aic-pod/libs/mcpx"
	"github.com/veypi/aic-pod/protocol"

	"github.com/veypi/vbox"
)

func (c *Client) configureMCP(settings mcpx.Settings) error {
	servers, err := mcpServers(settings.Servers, c.options().WorkDir)
	if err != nil {
		return err
	}
	// TCP 条目（url = tcp://…）不是 MCP 传输：不进连接管理器，由 vtools
	// 注册层直接接管（syncServiceRegistrations）。
	mcpSet := make(map[string]mcpx.Config, len(servers))
	for name, cfg := range servers {
		if cfg.IsTCP() {
			continue
		}
		mcpSet[name] = cfg
	}
	manager, err := mcpx.NewManager(mcpSet, mcpx.Options{Authorize: c.authorizeMCP, HTTPClient: c.mcpHTTP(), Processes: c.procs, Logf: c.logf, Expired: c.mcpExpired, Policy: func(cfg mcpx.Config) vbox.Policy {
		return c.nativePolicy(context.Background(), cfg.Cwd, cfg.Command)
	}})
	if err != nil {
		return err
	}
	c.svc.mu.Lock()
	if c.svc.browserCancel != nil {
		c.svc.browserCancel()
	}
	if c.svc.manager != nil {
		old := c.svc.manager
		old.Close()
		if old.Started("browser") {
			c.closeBrowser(c.svc.browser)
		}
	}
	c.svc.browser = nil
	if _, overridden := settings.Servers["browser"]; !overridden {
		browser := servers["browser"]
		c.svc.browser = &browser
	}
	c.svc.owner = make(map[string]mcpx.Config, len(settings.Servers))
	for name, cfg := range settings.Servers {
		c.svc.owner[name] = cfg
	}
	c.svc.browserCtx, c.svc.browserCancel = context.WithCancel(context.Background())
	c.svc.manager = manager
	c.svc.mu.Unlock()
	// 服务登记为 vsh 根命令（vtools；含 TCP 条目与 browser 流通道）。
	c.syncServiceRegistrations(servers)
	return nil
}

// mcpExpired 在服务因空闲有效期被回收后释放其进程之外的资源：内置 browser
// 的 MCP 前端退出不会结束上游 daemon，需显式关掉受管浏览器；所有者覆盖的
// browser 项自管 daemon，不在此列。
func (c *Client) mcpExpired(name string) {
	if name != "browser" {
		return
	}
	c.svc.mu.RLock()
	config := c.svc.browser
	c.svc.mu.RUnlock()
	c.closeBrowser(config)
}

func (c *Client) mcpSession(ctx context.Context, server string) (*mcp.ClientSession, error) {
	if !c.execAllowed(execution.SessionFromContext(ctx), "mcp."+server) {
		return nil, fmt.Errorf("mcp.%s: denied by exec rules; request grant cmd mcp.%s", server, server)
	}
	c.svc.mu.RLock()
	manager := c.svc.manager
	c.svc.mu.RUnlock()
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
