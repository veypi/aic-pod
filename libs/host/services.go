package host

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/mcpx"
	"github.com/veypi/aic-pod/libs/rtc"
	"github.com/veypi/aic-pod/libs/vtools"
	"github.com/veypi/aic-pod/protocol"
)

// 本文件是 vtools 注册层的 host 接线（docs/reference/host-vtools.md）：
// 服务（MCP / TCP / …）登记为 vsh 根命令，mcp 命令退化为管理面。

// serviceStreamGate 只保护独立 RTC 流通道；vsh 命令由统一 exec 准入处理。
func (c *Client) serviceStreamGate(_ context.Context, session, alias string) error {
	if !c.execAllowed(session, "mcp."+alias) {
		return fmt.Errorf("mcp.%s: denied by exec rules; request grant cmd mcp.%s", alias, alias)
	}
	return nil
}

// mcpResolve 现取当前管理器的共享连接：管理器随配置更新重建时，已注册的
// 服务命令无需重注册。
func (c *Client) mcpResolve(name string) vtools.MCPResolve {
	return func(ctx context.Context) (*mcp.ClientSession, error) {
		c.svc.mu.RLock()
		manager := c.svc.manager
		c.svc.mu.RUnlock()
		if manager == nil {
			return nil, fmt.Errorf("MCP unavailable")
		}
		return manager.Session(ctx, name)
	}
}

// mcpManager 返回当前连接管理器（可能为 nil——配置更新期间）。
func (c *Client) mcpManager() *mcpx.Manager {
	c.svc.mu.RLock()
	defer c.svc.mu.RUnlock()
	return c.svc.manager
}

// serviceRegistered 报告 alias 是否是已登记的服务命令（commands 动态展示用）。
func (c *Client) serviceRegistered(name string) bool {
	return c.svc.registry != nil && c.svc.registry.Has(name)
}

// serviceRegistry 返回服务登记表（nil = 工具未初始化）。
func (c *Client) serviceRegistry() *vtools.Registry { return c.svc.registry }

// tcpAddr 报告已登记 Provider 的 TCP 端点（非 TCP 形态 = false）。按接口判定
// 而非具体类型：宿主在 TCPProvider 之上还有包装（blenderProvider），断言
// *TCPProvider 会把它们误认成非 TCP 服务。
func tcpAddr(p vtools.Provider) (string, bool) {
	ap, ok := p.(vtools.AddrProvider)
	if !ok {
		return "", false
	}
	return ap.Addr(), true
}

// registerService 登记单个服务为 vsh 根命令并返回失败原因（禁用 = 无事发生）。
// 同名已登记时按「已登记 provider 的形态 vs 新配置形态」判定：
// MCP→MCP 不动（适配器经 mcpResolve 现取连接，管理器重建透明）；TCP→同址 TCP
// 不动；其余（TCP↔MCP 换型、TCP 换址）摘除重登记——TCP 端点钉在 provider
// 实例上，不能留着旧地址继续服务。
func (c *Client) registerService(name string, cfg mcpx.Config) error {
	hub := c.svc.registry
	if hub == nil {
		return fmt.Errorf("vtools %s: registry unavailable", name)
	}
	if cfg.Disabled {
		return nil
	}
	if hub.Has(name) {
		existing, _ := hub.Lookup(name)
		addr, existingTCP := tcpAddr(existing)
		switch {
		case cfg.IsTCP() && existingTCP && addr == strings.TrimPrefix(cfg.URL, "tcp://"):
			return nil // 同一 TCP 端点
		case !cfg.IsTCP() && !existingTCP:
			return nil // MCP→MCP
		}
		hub.Unregister(name)
	}
	var p vtools.Provider
	if cfg.IsTCP() {
		tcp, err := vtools.NewTCP(strings.TrimPrefix(cfg.URL, "tcp://"), vtools.WithToken(cfg.Token))
		if err != nil {
			return fmt.Errorf("vtools %s: %w", name, err)
		}
		p = tcp
	} else {
		p = vtools.MCP(c.mcpResolve(name))
	}
	if err := hub.Register(name, p); err != nil {
		return fmt.Errorf("vtools %s: %w", name, err)
	}
	return nil
}

// syncServiceRegistrations 全量同步服务集合（启动/配置更新时调用）：新增注册、
// 禁用或消失的摘除；已登记的同名服务不动（MCP 适配器经 mcpResolve 现取连接，
// 管理器重建对其透明）。内置 browser 的实时画面通道随配置状态挂上/摘除。
// 登记失败的服务（撞名/地址无效）不影响其余服务，被拒清单随之从头重算。
func (c *Client) syncServiceRegistrations(servers map[string]mcpx.Config) {
	hub := c.svc.registry
	if hub == nil {
		return
	}
	c.resetServiceRejections()
	for name, cfg := range servers {
		// 撞名（平台命令/内建）或配置无效：跳过该服务，记日志 + mcp list 曝光
		if err := c.registerService(name, cfg); err != nil {
			c.logf("%v", err)
			c.recordServiceRejection(name, cfg, err)
		}
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
	c.svc.mu.RLock()
	builtin := c.svc.browser != nil
	c.svc.mu.RUnlock()
	if cfg, ok := servers["browser"]; builtin && ok && !cfg.Disabled {
		if err := hub.RegisterStream("browser", vtools.StreamerFunc{Channel: protocol.RtcBrowserChannel, Relay: c.streamBrowser}); err != nil {
			c.logf("vtools browser stream: %v", err)
		}
	} else {
		hub.UnregisterStream("browser")
	}
}

// serviceRejection 是一次登记失败的服务（配置里的 alias 撞平台命令名、地址
// 无效等）：命令面没有它但配置还在——mcp list 以 state=rejected 曝光，避免
// 「配了却没有」只存在于 pod 日志。
type serviceRejection struct {
	kind   string // mcp | tcp（按配置形态）
	reason string
}

// recordServiceRejection 记下一次被拒的服务登记。
func (c *Client) recordServiceRejection(name string, cfg mcpx.Config, err error) {
	kind := "mcp"
	if cfg.IsTCP() {
		kind = "tcp"
	}
	c.svc.mu.Lock()
	defer c.svc.mu.Unlock()
	if c.svc.rejected == nil {
		c.svc.rejected = map[string]serviceRejection{}
	}
	c.svc.rejected[name] = serviceRejection{kind: kind, reason: err.Error()}
}

// clearServiceRejection 摘除一条被拒记录（配置里的条目被移除时）。
func (c *Client) clearServiceRejection(name string) {
	c.svc.mu.Lock()
	defer c.svc.mu.Unlock()
	delete(c.svc.rejected, name)
}

// resetServiceRejections 清空被拒清单（全量同步开头重算）。
func (c *Client) resetServiceRejections() {
	c.svc.mu.Lock()
	defer c.svc.mu.Unlock()
	c.svc.rejected = map[string]serviceRejection{}
}

// serviceRejections 返回被拒清单快照。
func (c *Client) serviceRejections() map[string]serviceRejection {
	c.svc.mu.RLock()
	defer c.svc.mu.RUnlock()
	out := make(map[string]serviceRejection, len(c.svc.rejected))
	for name, rej := range c.svc.rejected {
		out[name] = rej
	}
	return out
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
	c.svc.mu.RLock()
	defer c.svc.mu.RUnlock()
	_, ok := c.svc.owner[alias]
	return ok
}

// ownerConfigs 返回所有者显式配置的快照。
func (c *Client) ownerConfigs() map[string]mcpx.Config {
	c.svc.mu.RLock()
	defer c.svc.mu.RUnlock()
	out := make(map[string]mcpx.Config, len(c.svc.owner))
	for name, cfg := range c.svc.owner {
		out[name] = cfg
	}
	return out
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
	if c.svc.registry == nil {
		return nil, false
	}
	relay, ok := c.svc.registry.StreamHandler(label)
	if !ok {
		return nil, false
	}
	return rtc.StreamRelay(relay), true
}
