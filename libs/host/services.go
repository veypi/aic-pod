package host

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/mcpx"
	"github.com/veypi/aic-pod/libs/rtc"
	"github.com/veypi/aic-pod/libs/vtools"
	"github.com/veypi/vsh/commands"
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

// registerService 登记所有者配置的服务。所有者覆盖不继承内置 pack 的流声明。
func (c *Client) registerService(name string, cfg mcpx.Config) error {
	c.svc.registrationMu.Lock()
	defer c.svc.registrationMu.Unlock()
	return c.registerServiceLocked(name, cfg)
}

// registerServiceLocked 供已持有 registrationMu 的完整服务变更流程调用。
func (c *Client) registerServiceLocked(name string, cfg mcpx.Config) error {
	return c.registerServiceWithManifest(name, cfg, nil)
}

// unregisterService 与全量同步/新增共用登记锁，避免并发增删留下半套路由。
func (c *Client) unregisterService(name string) {
	c.svc.registrationMu.Lock()
	defer c.svc.registrationMu.Unlock()
	c.unregisterServiceLocked(name)
}

// unregisterServiceLocked 供已持有 registrationMu 的完整服务变更流程调用。
func (c *Client) unregisterServiceLocked(name string) {
	if c.svc.registry != nil {
		c.svc.registry.Unregister(name)
	}
}

// manifestStreamer 把 pack 的通道声明接到真实 Streamer。通道只是路由：除
// browser 的 pod 内置转发适配器外，Provider 必须自己实现 Streamer；仅写
// manifest 不会凭空获得流能力。nil manifest = 所有者配置，不使用 pack 声明。
func (c *Client) manifestStreamer(alias string, p vtools.Provider, m *packManifest) (vtools.Streamer, error) {
	if m == nil || m.Stream == nil {
		return nil, nil
	}
	channel := m.Stream.Channel
	if err := vtools.ValidateStreamChannel(channel); err != nil {
		return nil, fmt.Errorf("pack %s: stream channel %q: %w", alias, channel, err)
	}
	if alias == "browser" {
		return vtools.StreamerFunc{Channel: channel, Relay: c.streamBrowser}, nil
	}
	s, ok := p.(vtools.Streamer)
	if !ok {
		return nil, fmt.Errorf("pack %s: stream channel %q declared but provider does not implement vtools.Streamer", alias, channel)
	}
	return vtools.StreamerFunc{Channel: channel, Relay: s.Stream}, nil
}

// registerServiceProvider 原子登记命令与 manifest 流。m != nil 时流路由完全由
// manifest 决定：无 stream 声明就不暴露 Provider 自带标签；有声明则覆盖其标签。
func (c *Client) registerServiceProvider(alias string, p vtools.Provider, cmd commands.Command, m *packManifest) error {
	hub := c.svc.registry
	if hub == nil {
		return fmt.Errorf("vtools %s: registry unavailable", alias)
	}
	if m == nil {
		if cmd != nil {
			return hub.RegisterCustom(alias, p, cmd)
		}
		return hub.Register(alias, p)
	}
	streamer, err := c.manifestStreamer(alias, p, m)
	if err != nil {
		return err
	}
	if cmd != nil {
		return hub.RegisterCustomWithStream(alias, p, cmd, streamer)
	}
	return hub.RegisterWithStream(alias, p, streamer)
}

// registerServiceWithManifest 登记单个服务并返回失败原因（禁用 = 无事发生）。
// 同形态且同流标签可复用；MCP 适配器现取管理器连接。换型、换址、增删流或换
// 流标签都摘除重登记，不能保留旧 TCP 地址或旧 RTC 路由。
func (c *Client) registerServiceWithManifest(name string, cfg mcpx.Config, m *packManifest) error {
	hub := c.svc.registry
	if hub == nil {
		return fmt.Errorf("vtools %s: registry unavailable", name)
	}
	if cfg.Disabled {
		return nil
	}
	// 即使服务形态没变，非法流声明也必须显式拒绝，不能因复用而静默通过。
	if m != nil && m.Stream != nil {
		if err := vtools.ValidateStreamChannel(m.Stream.Channel); err != nil {
			return fmt.Errorf("pack %s: stream channel %q: %w", name, m.Stream.Channel, err)
		}
	}
	if hub.Has(name) {
		existing, _ := hub.Lookup(name)
		addr, existingTCP := tcpAddr(existing)
		sameProvider := false
		switch {
		case cfg.IsTCP() && existingTCP && addr == strings.TrimPrefix(cfg.URL, "tcp://"):
			sameProvider = true
		case !cfg.IsTCP() && !existingTCP:
			sameProvider = true
		}
		desiredChannel := ""
		if m != nil && m.Stream != nil {
			desiredChannel = m.Stream.Channel
		}
		currentChannel, hasStream := hub.ServiceStreamChannel(name)
		sameStream := desiredChannel == "" && !hasStream
		if desiredChannel != "" {
			sameStream = hasStream && currentChannel == desiredChannel
		}
		if sameProvider && sameStream {
			return nil
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
	if err := c.registerServiceProvider(name, p, nil, m); err != nil {
		return fmt.Errorf("vtools %s: %w", name, err)
	}
	return nil
}

// syncServiceRegistrations 全量同步服务集合（启动/配置更新时调用）：新增注册、
// 禁用或消失的摘除；manifest 流随服务原子登记。排序使通道撞名的保留者与
// map 迭代顺序无关。登记失败不影响其余服务，被拒清单从头重算。
func (c *Client) syncServiceRegistrations(servers map[string]mcpx.Config) {
	c.svc.registrationMu.Lock()
	defer c.svc.registrationMu.Unlock()
	c.syncServiceRegistrationsLocked(servers)
}

// syncServiceRegistrationsLocked 供 configureMCP 在持有登记锁时完成整个
// manager/owner/registry 快照切换，避免与运行期 add/remove 交错。
func (c *Client) syncServiceRegistrationsLocked(servers map[string]mcpx.Config) {
	hub := c.svc.registry
	if hub == nil {
		return
	}
	packs, _ := builtinPacks()
	c.resetServiceRejections()
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		cfg := servers[name]
		var manifest *packManifest
		if !c.ownerConfigured(name) {
			manifest = packs[name]
		}
		// 撞名、非法流声明或通道冲突：跳过该服务，记日志 + mcp list 曝光
		if err := c.registerServiceWithManifest(name, cfg, manifest); err != nil {
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
