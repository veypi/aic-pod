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
	// 从读取目标配置到 manager/owner/registry 切换完成是一个线性化过程；
	// 否则并发 add/remove 会被旧 servers 快照误删或复活。
	c.svc.registrationMu.Lock()
	defer c.svc.registrationMu.Unlock()
	// 服务集就是所有者配置：pod 不再内置任何包的默认项（cua/blender/browser
	// 都是普通技能包，经统一 commands 机制注册）。
	servers := mcpServers(settings.Servers)
	// TCP 条目（url = tcp://…）不是 MCP 传输：不进连接管理器，由 vtools
	// 注册层直接接管（syncServiceRegistrations）。
	mcpSet := make(map[string]mcpx.Config, len(servers))
	for name, cfg := range servers {
		if cfg.IsTCP() {
			continue
		}
		mcpSet[name] = cfg
	}
	manager, err := mcpx.NewManager(mcpSet, mcpx.Options{Authorize: c.authorizeMCP, HTTPClient: c.mcpHTTP(), Processes: c.procs, Logf: c.logf, Policy: func(cfg mcpx.Config) vbox.Policy {
		return c.nativePolicy(context.Background(), cfg.Cwd, cfg.Command)
	}})
	if err != nil {
		return err
	}
	c.svc.mu.Lock()
	if c.svc.manager != nil {
		c.svc.manager.Close()
	}
	c.svc.owner = make(map[string]mcpx.Config, len(settings.Servers))
	for name, cfg := range settings.Servers {
		c.svc.owner[name] = cfg
	}
	c.svc.manager = manager
	c.svc.mu.Unlock()
	// 服务登记为 vsh 根命令（vtools；含 TCP 条目）。
	c.syncServiceRegistrationsLocked(servers)
	return nil
}

// mcpServers 返回所有者配置的服务集：pod 不再合并任何内置包的默认项。
func mcpServers(configured map[string]mcpx.Config) map[string]mcpx.Config {
	servers := make(map[string]mcpx.Config, len(configured))
	for name, cfg := range configured {
		servers[name] = cfg
	}
	return servers
}

func (c *Client) mcpSession(ctx context.Context, server string) (*mcp.ClientSession, error) {
	if c.cmds != nil && c.cmds.has(server) {
		if !c.execAllowed(execution.SessionFromContext(ctx), server) {
			return nil, fmt.Errorf("%s: denied by exec rules; request grant cmd %s", server, server)
		}
		return c.cmds.session(ctx, server)
	}
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
