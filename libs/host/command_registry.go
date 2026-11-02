package host

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/veypi/aic-pod/libs/execution"
	"github.com/veypi/aic-pod/libs/mcpx"
	"github.com/veypi/aic-pod/libs/rtc"
	"github.com/veypi/aic-pod/libs/vtools"
	"github.com/veypi/aic-pod/protocol"
	"github.com/veypi/vbox"
	"github.com/veypi/vsh/commands"
)

type commandStreamBinding struct {
	def     *hostCommand
	stream  hostStream
	manager *mcpx.Manager
	process *streamProcess
}

type commandHub struct {
	client *Client
	reg    *commands.Registry

	rebuildMu sync.Mutex
	mu        sync.RWMutex
	active    map[string]*hostCommand
	rejected  map[string]string
	streams   map[string]commandStreamBinding
	managers  map[string]*mcpx.Manager
	processes map[string]*streamProcess
	runtime   map[string]*hostCommand
}

func newCommandHub(client *Client, reg *commands.Registry) *commandHub {
	return &commandHub{
		client: client, reg: reg,
		active: map[string]*hostCommand{}, rejected: map[string]string{},
		streams: map[string]commandStreamBinding{}, managers: map[string]*mcpx.Manager{},
		processes: map[string]*streamProcess{}, runtime: map[string]*hostCommand{},
	}
}

func (h *commandHub) has(alias string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.active[alias]
	return ok
}

func (h *commandHub) definition(alias string) (*hostCommand, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	def, ok := h.active[alias]
	return def, ok
}

// rejection 返回 alias 最近一次装配被拒绝的原因；不在 rejected 里则为空。
func (h *commandHub) rejection(alias string) string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.rejected[alias]
}

func (h *commandHub) upsertRuntime(def *hostCommand) error {
	h.rebuildMu.Lock()
	defer h.rebuildMu.Unlock()
	h.runtime[def.ID] = def
	return h.reloadLocked()
}

func (h *commandHub) removeRuntime(id string) error {
	h.rebuildMu.Lock()
	defer h.rebuildMu.Unlock()
	delete(h.runtime, id)
	return h.reloadLocked()
}

// hasRuntime 报告运行期定义是否存在。runtime map 的写方（upsertRuntime /
// removeRuntime）都在 rebuildMu 下，读方必须同锁——否则并发 add/remove 会触发
// Go 运行时的 concurrent map read and map write（不可 recover，进程直死）。
func (h *commandHub) hasRuntime(id string) bool {
	h.rebuildMu.Lock()
	defer h.rebuildMu.Unlock()
	_, ok := h.runtime[id]
	return ok
}

func (h *commandHub) reload() error {
	h.rebuildMu.Lock()
	defer h.rebuildMu.Unlock()
	return h.reloadLocked()
}

func (h *commandHub) reloadLocked() error {
	defs, err := h.client.collectHostCommands(h.runtime)
	if err != nil {
		return err
	}
	return h.rebuildLocked(defs)
}

// inheritCommandStreams 保留同一命令定义的流地址。地址属于常驻进程的运行态，
// 不是每次注册表重建都要变化的声明；只有增删流或修改 address_env 才重新分配。
func inheritCommandStreams(old, next *hostCommand) bool {
	if old == nil || old.ID != next.ID || len(old.Streams) != len(next.Streams) {
		return false
	}
	for name, stream := range next.Streams {
		previous, ok := old.Streams[name]
		if !ok || previous.Name != stream.Name || previous.AddressEnv != stream.AddressEnv || previous.Address == "" {
			return false
		}
	}
	for name, stream := range next.Streams {
		stream.Address = old.Streams[name].Address
		next.Streams[name] = stream
	}
	return true
}

func (h *commandHub) rebuildLocked(defs map[string]*hostCommand) error {
	winners := map[string]*hostCommand{}
	skillAliases := map[string]bool{}
	ids := make([]string, 0, len(defs))
	for id := range defs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		def := defs[id]
		if def.Source == commandSourceSkill {
			skillAliases[def.Alias] = true
		}
		current := winners[def.Alias]
		if current == nil || def.Order > current.Order {
			winners[def.Alias] = def
		}
	}
	for alias, def := range winners {
		if def.Source == commandSourceConfig && skillAliases[alias] {
			delete(winners, alias)
		}
	}

	h.mu.RLock()
	oldActive := h.active
	oldManagers := h.managers
	oldProcesses := h.processes
	h.mu.RUnlock()

	aliases := make([]string, 0, len(winners))
	for alias := range winners {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	active := map[string]*hostCommand{}
	rejected := map[string]string{}
	streamBindings := map[string]commandStreamBinding{}
	processes := map[string]*streamProcess{}
	managers := map[string]*mcpx.Manager{}
	prepared := map[string]*hostCommand{}

	for _, alias := range aliases {
		def := winners[alias].clone()
		if !inheritCommandStreams(oldActive[alias], def) {
			if err := allocateCommandStreams(def); err != nil {
				rejected[alias] = err.Error()
				continue
			}
		}
		if err := validateHostCommand(def); err != nil {
			rejected[alias] = err.Error()
			continue
		}
		if oldActive[alias] == nil {
			if _, taken := h.reg.Lookup(alias); taken {
				rejected[alias] = "command name is taken by a platform or legacy service"
				continue
			}
		}
		prepared[alias] = def
	}

	managerOptions := mcpx.Options{
		Authorize: func(ctx context.Context, alias, _ string, _ mcp.Params) error {
			if !h.client.execAllowed(execution.SessionFromContext(ctx), alias) {
				return protocol.Fail("permission_denied", "Command denied; request grant cmd "+alias)
			}
			return nil
		},
		HTTPClient: h.client.mcpHTTP(), Processes: h.client.procs, Logf: h.client.logf,
		Policy: func(config mcpx.Config) vbox.Policy {
			return h.client.nativePolicy(context.Background(), config.Cwd, config.Command)
		},
	}
	createdManagers := map[*mcpx.Manager]bool{}
	for _, alias := range aliases {
		def := prepared[alias]
		if def == nil || def.Config.Protocol != "mcp" {
			continue
		}
		if oldManagers[alias] != nil && reflect.DeepEqual(oldActive[alias], def) {
			managers[alias] = oldManagers[alias]
			continue
		}
		manager, err := mcpx.NewManager(map[string]mcpx.Config{alias: def.mcpConfig()}, managerOptions)
		if err != nil {
			for created := range createdManagers {
				_ = created.Close()
			}
			return err
		}
		createdManagers[manager] = true
		managers[alias] = manager
	}

	for alias := range oldActive {
		h.reg.Unregister(alias)
	}
	for _, alias := range aliases {
		def := prepared[alias]
		if def == nil {
			continue
		}
		var cmd commands.Command
		switch def.kind() {
		case "mcp":
			manager := managers[alias]
			provider := vtools.MCP(func(ctx context.Context) (*mcp.ClientSession, error) {
				return manager.Session(ctx, alias)
			})
			cmd = commands.DefineCommand(alias, func(ctx context.Context, inv *commands.Invocation) error {
				return vtools.RunProvider(ctx, inv, alias, provider)
			})
		case "stream":
			cmd = commands.DefineCommand(alias, func(_ context.Context, inv *commands.Invocation) error {
				return commands.Exitf(inv, 2, "%s: command only provides streams", alias)
			})
		default:
			cmd = commands.DefineCommand(alias, func(ctx context.Context, inv *commands.Invocation) error {
				return h.client.runHostCommand(ctx, def, inv)
			})
		}
		if err := h.reg.RegisterGuarded(cmd); err != nil {
			rejected[alias] = err.Error()
			continue
		}
		active[alias] = def
		if len(def.Streams) > 0 {
			if def.Config.Protocol == "" {
				if oldProcesses[alias] != nil && reflect.DeepEqual(oldActive[alias], def) {
					processes[alias] = oldProcesses[alias]
				} else {
					processes[alias] = newStreamProcess(h.client, def)
				}
			}
			for _, stream := range def.Streams {
				// 流与命令同属一个命名空间：地址 = 命令别名 + 流名。
				// skill_id 是平台内部键，不进寻址（同名命令按注册表的生效定义解析）。
				label := def.Alias + ":" + stream.Name
				streamBindings[label] = commandStreamBinding{def: def, stream: stream, manager: managers[alias], process: processes[alias]}
			}
		}
	}

	for alias, manager := range managers {
		if active[alias] != nil {
			continue
		}
		if createdManagers[manager] {
			_ = manager.Close()
		}
		delete(managers, alias)
	}

	h.mu.Lock()
	h.active, h.rejected, h.streams = active, rejected, streamBindings
	h.managers, h.processes = managers, processes
	h.mu.Unlock()

	for alias, old := range oldManagers {
		if managers[alias] != old {
			_ = old.Close()
		}
	}
	for alias, old := range oldProcesses {
		if processes[alias] != old {
			old.stop()
		}
	}
	return nil
}

func (h *commandHub) session(ctx context.Context, alias string) (*mcp.ClientSession, error) {
	h.mu.RLock()
	def, manager := h.active[alias], h.managers[alias]
	h.mu.RUnlock()
	if def == nil || def.Config.Protocol != "mcp" || manager == nil {
		return nil, fmt.Errorf("%s is not an MCP command", alias)
	}
	return manager.Session(ctx, alias)
}

func (h *commandHub) startStop(ctx context.Context, inv *commands.Invocation, op, alias string) error {
	h.mu.RLock()
	def, manager, process := h.active[alias], h.managers[alias], h.processes[alias]
	h.mu.RUnlock()
	if def == nil {
		return commands.Exitf(inv, 1, "commands %s: unknown command %q", op, alias)
	}
	if !def.resident() {
		return commands.Exitf(inv, 2, "commands %s: one-shot process does not support lifecycle", op)
	}
	if def.Config.Protocol == "mcp" {
		if op != "start" {
			if err := manager.Restart(ctx, alias); err != nil {
				return commands.Exitf(inv, 1, "commands %s: %s", op, err)
			}
		}
		if op != "stop" {
			if _, err := manager.Session(ctx, alias); err != nil {
				return commands.Exitf(inv, 1, "commands %s: %s", op, err)
			}
		}
	} else {
		if op == "stop" {
			process.stop()
		} else {
			if op == "restart" {
				process.stop()
			}
			if err := process.start(); err != nil {
				return commands.Exitf(inv, 1, "commands %s: %s", op, err)
			}
		}
	}
	fmt.Fprintf(inv.Stdout, "%s %s\n", op, alias)
	return nil
}

func (h *commandHub) streamResolver(label string) (rtc.StreamRelay, bool) {
	h.mu.RLock()
	binding, ok := h.streams[label]
	h.mu.RUnlock()
	if !ok {
		return nil, false
	}
	return func(ctx context.Context, caller protocol.Caller, input <-chan []byte, send func([]byte) error) error {
		return h.client.streamHostCommand(ctx, caller, binding, input, send)
	}, true
}

func (h *commandHub) close() {
	h.rebuildMu.Lock()
	defer h.rebuildMu.Unlock()
	h.mu.RLock()
	managers, processes := h.managers, h.processes
	h.mu.RUnlock()
	for _, manager := range managers {
		_ = manager.Close()
	}
	for _, process := range processes {
		process.stop()
	}
}
