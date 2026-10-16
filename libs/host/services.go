package host

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/execution"
	"github.com/veypi/aic-pod/libs/mcpx"
	"github.com/veypi/aic-pod/libs/rtc"
	"github.com/veypi/aic-pod/libs/vtools"
	"github.com/veypi/aic-pod/protocol"
)

// 本文件是 vtools 注册层的 host 接线（docs/host-vtools.md）：
// 服务（MCP / TCP / …）登记为 vsh 根命令，mcp 命令退化为管理面。

// mcpGate 是工具命令与流通道的调用门：exec 规则 mcp.<alias>（与原
// mcpSession 门同一条规则，授权语义不变——grant cmd mcp.<alias> 放行该
// 服务的全部工具与流）。
func (c *Client) mcpGate(ctx context.Context, alias string) error {
	if !c.execAllowed(execution.SessionFromContext(ctx), "mcp."+alias) {
		return fmt.Errorf("mcp.%s: denied by exec rules; request grant cmd mcp.%s", alias, alias)
	}
	return nil
}

// mcpResolve 现取当前管理器的共享连接：管理器随配置更新重建时，已注册的
// 服务命令无需重注册。
func (c *Client) mcpResolve(name string) vtools.MCPResolve {
	return func(ctx context.Context) (*mcp.ClientSession, error) {
		c.mcpMu.RLock()
		manager := c.mcpServices
		c.mcpMu.RUnlock()
		if manager == nil {
			return nil, fmt.Errorf("MCP unavailable")
		}
		return manager.Session(ctx, name)
	}
}

// mcpManager 返回当前连接管理器（可能为 nil——配置更新期间）。
func (c *Client) mcpManager() *mcpx.Manager {
	c.mcpMu.RLock()
	defer c.mcpMu.RUnlock()
	return c.mcpServices
}

// serviceRegistered 报告 alias 是否是已登记的服务命令（commands 动态展示用）。
func (c *Client) serviceRegistered(name string) bool {
	return c.services != nil && c.services.Has(name)
}

// registerService 登记单个服务为 vsh 根命令（禁用/撞名跳过并记日志）。
// configureMCP 的全量同步与 mcp add 的单条登记共用此入口。同名已登记时：
// MCP 适配器经 mcpResolve 现取连接（管理器重建透明）不动；TCP 条目的地址
// 钉在 provider 上，配置变了摘除重登记。
func (c *Client) registerService(name string, cfg mcpx.Config) {
	hub := c.services
	if hub == nil || cfg.Disabled {
		return
	}
	if hub.Has(name) {
		if !cfg.IsTCP() {
			return
		}
		if p, ok := hub.Lookup(name); ok {
			if tcp, isTCP := p.(*vtools.TCPProvider); isTCP && tcp.Addr() == strings.TrimPrefix(cfg.URL, "tcp://") {
				return // 地址没变
			}
		}
		hub.Unregister(name)
	}
	var p vtools.Provider
	if cfg.IsTCP() {
		tcp, err := vtools.NewTCP(strings.TrimPrefix(cfg.URL, "tcp://"))
		if err != nil {
			c.logf("vtools %s: %v", name, err)
			return
		}
		p = tcp
	} else {
		p = vtools.MCP(c.mcpResolve(name))
	}
	if err := hub.Register(name, p); err != nil {
		c.logf("vtools %s: %v", name, err)
	}
}

// syncServiceRegistrations 全量同步服务集合（启动/配置更新时调用）：新增注册、
// 禁用或消失的摘除；已登记的同名服务不动（MCP 适配器经 mcpResolve 现取连接，
// 管理器重建对其透明）。内置 browser 的实时画面通道随配置状态挂上/摘除。
func (c *Client) syncServiceRegistrations(servers map[string]mcpx.Config) {
	hub := c.services
	if hub == nil {
		return
	}
	for name, cfg := range servers {
		c.registerService(name, cfg)
	}
	for _, alias := range hub.Aliases() {
		cfg, ok := servers[alias]
		if !ok || cfg.Disabled {
			// 内置非 MCP 服务（blender 等 TCP 包）不在 servers 集里，不是「消失」
			if !ok && c.isBuiltinPack(alias) && !c.ownerConfigured(alias) {
				continue
			}
			hub.Unregister(alias)
		}
	}
	// 内置 browser 的流通道：所有者覆盖/禁用时不挂（streamBrowser 本身也要求
	// 内置配置在场，这里保持登记与语义一致）。
	c.mcpMu.RLock()
	builtin := c.browserConfig != nil
	c.mcpMu.RUnlock()
	if cfg, ok := servers["browser"]; builtin && ok && !cfg.Disabled {
		if err := hub.RegisterStream("browser", vtools.StreamerFunc{Channel: protocol.RtcBrowserChannel, Relay: c.streamBrowser}); err != nil {
			c.logf("vtools browser stream: %v", err)
		}
	} else {
		hub.UnregisterStream("browser")
	}
}

// persistMCPServer 持久化服务条目（config=nil 表示删除）：只写 config.yaml，
// 运行态已由调用方先行生效。
func (c *Client) persistMCPServer(alias string, config *mcpx.Config) error {
	unlock := cfg.LockUpdate()
	defer unlock()
	fileCfg, err := cfg.LoadFile()
	if err != nil {
		return err
	}
	if fileCfg.MCP.Servers == nil {
		fileCfg.MCP.Servers = map[string]mcpx.Config{}
	}
	if config == nil {
		delete(fileCfg.MCP.Servers, alias)
	} else {
		fileCfg.MCP.Servers[alias] = *config
	}
	return cfg.Save(fileCfg)
}

// ownerConfigured 报告服务是否由设备所有者显式配置（内置默认项不在此列）。
func (c *Client) ownerConfigured(alias string) bool {
	_, ok := c.options().MCP.Servers[alias]
	return ok
}

// isBuiltinPack 报告 alias 是否是内嵌技能包声明的内置服务。
func (c *Client) isBuiltinPack(alias string) bool {
	packs, err := builtinPacks()
	if err != nil {
		return false
	}
	_, ok := packs[alias]
	return ok
}

// streamResolver 是 rtc.Config.Streams 的装配：按 DataChannel 标签现查
// vtools 登记表（通道开启时解析，运行期 add 的流式服务无需重启 RTC）。
func (c *Client) streamResolver(label string) (rtc.StreamRelay, bool) {
	if c.services == nil {
		return nil, false
	}
	relay, ok := c.services.StreamHandler(label)
	if !ok {
		return nil, false
	}
	return rtc.StreamRelay(relay), true
}
