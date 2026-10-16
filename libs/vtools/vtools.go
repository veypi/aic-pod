// Package vtools 是协议无关的工具注册层：任何协议（MCP / TCP / …）适配成
// Provider 后即可注册为 vsh 根命令——`mcp` 命令因此退化为纯管理面
// （add/remove/start/stop/list），工具调用不再经它中转。
//
// 形态（已定稿，见 docs/host-vtools.md）：
//   - 每个服务 alias 注册一个 vsh 根命令（RegisterGuarded 撞名即拒）：
//     `<alias> <tool> [--flag value]... [--input JSON|-] [--json]`；
//     无参打印工具目录；`<alias> <tool> --help` 按 inputSchema 渲染参数表。
//   - 注册即时、连接懒惰：命令进 Registry 不 connect，首次执行才连接。
//   - 调用门在命令边界（Gate，host 接线 exec 规则 mcp.<alias>）；Provider
//     层不再各自设门。
//   - Streamer 是可选能力：实现它的服务获得一条 RTC 专用 DataChannel
//     （消息边界保持的纯转发，权限复查与保活由实现方负责）。
package vtools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/veypi/aic-pod/libs/mcpx"
	"github.com/veypi/aic-pod/protocol"
	"github.com/veypi/vsh/commands"
)

// Tool 是能力面条目（协议无关；MCP 的 inputSchema 原样透传）。
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// Provider 是协议适配器的最小面。
type Provider interface {
	Tools(ctx context.Context) ([]Tool, error)
	Call(ctx context.Context, tool string, args map[string]any) (any, error)
}

// CallError 是工具级失败（如 MCP isError）：结果照常渲染，退出码 1。
type CallError struct {
	Result any
}

func (e *CallError) Error() string { return "tool reported an error" }

// ImagePreviewer 是可选能力：支持 --image-preview WIDTHxHEIGHT 的 Provider
// （MCP 适配器实现；其余协议默认不支持，显式报错）。
type ImagePreviewer interface {
	PreviewImages(result any, size string) (any, error)
}

// AddrProvider 是有明确 TCP 端点地址的 Provider（TCPProvider，以及宿主在它
// 之上包装的 Provider 如 blender）。登记层据此判定「已登记的服务是不是同一个
// TCP 端点」——换型或换址即摘除重登记；按接口而非具体类型判定，包装形态
// （blenderProvider）不会被误认成非 TCP 服务。
type AddrProvider interface {
	Addr() string
}

// StreamRelay 是流的转发执行体：消息边界保持的双向纯转发（input = 前端
// 发来的消息；send = 向前端投递），与 Call 的连接相互独立。
type StreamRelay func(ctx context.Context, caller protocol.Caller, input <-chan []byte, send func([]byte) error) error

// Streamer 是可选能力：有实时流的服务（或独立的流接线）实现它，RTC 层按
// StreamChannel 标签开 DataChannel。
type Streamer interface {
	StreamChannel() string
	Stream(ctx context.Context, caller protocol.Caller, input <-chan []byte, send func([]byte) error) error
}

// StreamerFunc 把独立的流接线（不与 Provider 同体）适配成 Streamer。
type StreamerFunc struct {
	Channel string
	Relay   StreamRelay
}

func (s StreamerFunc) StreamChannel() string { return s.Channel }

func (s StreamerFunc) Stream(ctx context.Context, caller protocol.Caller, input <-chan []byte, send func([]byte) error) error {
	return s.Relay(ctx, caller, input, send)
}

// Gate 是调用门：每次命令执行 / 流通道开启前检查（host 接线 exec 规则
// mcp.<alias>）。session = 会话键（会话级 temp 授权的查找键）：命令路径取
// Options.SessionOf(ctx)；流通道路径传 caller.Origin——peer ctx 派生自
// Background，不携带会话，直接用 ctx 取会让会话 grant 在开门处静默失效。
// nil = 放行。
type Gate func(ctx context.Context, session, alias string) error

type entry struct {
	provider Provider
	streamer Streamer
}

// Options 是登记表的装配参数。
type Options struct {
	// Gate 是调用门（命令执行 / 流通道开启前检查）。nil = 放行。
	Gate Gate
	// SessionOf 从命令执行上下文取会话键（host 注入 execution.SessionFromContext）：
	// 会话键是会话级临时授权的查找键，本层不假设它以什么形式挂在 ctx 上。
	// nil = 命令路径恒空串（无会话级授权区分）。
	SessionOf func(context.Context) string
	Logf      func(string, ...any)
}

// Registry 是服务登记表：vsh 命令注册与 RTC 流通道查询共用这一处。
type Registry struct {
	cmds      *commands.Registry
	gate      Gate
	sessionOf func(context.Context) string
	logf      func(string, ...any)

	mu       sync.RWMutex
	entries  map[string]*entry
	channels map[string]string // stream channel label → alias
}

func NewRegistry(cmds *commands.Registry, opts Options) *Registry {
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	if opts.SessionOf == nil {
		opts.SessionOf = func(context.Context) string { return "" }
	}
	return &Registry{
		cmds: cmds, gate: opts.Gate, sessionOf: opts.SessionOf, logf: opts.Logf,
		entries: map[string]*entry{}, channels: map[string]string{},
	}
}

// Register 把服务登记为 vsh 根命令（撞名即拒，绝不静默覆盖内建或平台命令）。
// 重复登记同一 alias 即拒——更新配置先 Unregister 再 Register。stream channel
// 标签同样撞名即拒（与命令名同一原则）。
func (r *Registry) Register(alias string, p Provider) error {
	return r.register(alias, p, nil)
}

// RegisterCustom 同 Register，但命令面由调用方提供（如 blender 的生命周期
// 动词：分发后把剩余参数透传给 Registry.RunTool）。cmd.Name() 必须等于
// alias——vsh registry 以 Name 为键、Unregister 按 alias 摘，不一致会驻留。
func (r *Registry) RegisterCustom(alias string, p Provider, cmd commands.Command) error {
	if cmd == nil {
		return fmt.Errorf("vtools %s: custom command is nil", alias)
	}
	if cmd.Name() != alias {
		return fmt.Errorf("vtools %s: custom command name %q does not match alias", alias, cmd.Name())
	}
	return r.register(alias, p, cmd)
}

func (r *Registry) register(alias string, p Provider, custom commands.Command) error {
	if err := mcpx.ValidateAlias(alias); err != nil {
		return err
	}
	if p == nil {
		return fmt.Errorf("vtools %s: provider is nil", alias)
	}
	var streamer Streamer
	if s, ok := p.(Streamer); ok {
		streamer = s
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.entries[alias]; exists {
		return fmt.Errorf("vtools %s: service already registered", alias)
	}
	if streamer != nil {
		label := streamer.StreamChannel()
		if label == "" {
			return fmt.Errorf("vtools %s: streamer requires a channel label", alias)
		}
		if occupant, taken := r.channels[label]; taken {
			return fmt.Errorf("vtools %s: stream channel %q is taken by %s", alias, label, occupant)
		}
	}
	cmd := custom
	if cmd == nil {
		cmd = commands.DefineCommand(alias, r.commandFunc(alias))
	}
	if err := r.cmds.RegisterGuarded(cmd); err != nil {
		return fmt.Errorf("vtools %s: command name is taken: %w", alias, err)
	}
	r.entries[alias] = &entry{provider: p, streamer: streamer}
	if streamer != nil {
		r.channels[streamer.StreamChannel()] = alias
	}
	return nil
}

// RegisterStream 给已登记的服务附加独立的流接线（如 browser：Provider 是
// MCP 适配器，流是 daemon 的原生 WebSocket 转发，两者不同体）。
func (r *Registry) RegisterStream(alias string, s Streamer) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[alias]
	if !ok {
		return fmt.Errorf("vtools %s: service is not registered", alias)
	}
	if s == nil || s.StreamChannel() == "" {
		return fmt.Errorf("vtools %s: streamer requires a channel label", alias)
	}
	if occupant, taken := r.channels[s.StreamChannel()]; taken && occupant != alias {
		return fmt.Errorf("vtools %s: stream channel %q is taken by %s", alias, s.StreamChannel(), occupant)
	}
	// 换标签：先摘本条目的旧映射——只写不删会让旧标签残留（不可复用，
	// 且开启时静默挂到新 streamer）。
	if e.streamer != nil && r.channels[e.streamer.StreamChannel()] == alias {
		delete(r.channels, e.streamer.StreamChannel())
	}
	e.streamer = s
	r.channels[s.StreamChannel()] = alias
	return nil
}

// UnregisterStream 摘除服务的流接线（保留工具命令）。
func (r *Registry) UnregisterStream(alias string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[alias]
	if !ok || e.streamer == nil {
		return
	}
	// 只摘自己的映射（撞名已在登记侧拒绝，这里是防御）
	if r.channels[e.streamer.StreamChannel()] == alias {
		delete(r.channels, e.streamer.StreamChannel())
	}
	e.streamer = nil
}

// Unregister 摘除服务：vsh 命令与流登记一并移除（在途调用/流持有各自的
// Command/Streamer 引用，自然跑完）；实现 io.Closer 的 Provider 顺带关闭
// （TCP 连接等）。缺席时幂等。
func (r *Registry) Unregister(alias string) {
	r.mu.Lock()
	e, ok := r.entries[alias]
	if ok {
		delete(r.entries, alias)
		if e.streamer != nil && r.channels[e.streamer.StreamChannel()] == alias {
			delete(r.channels, e.streamer.StreamChannel())
		}
	}
	r.mu.Unlock()
	if ok {
		r.cmds.Unregister(alias)
		if closer, isCloser := e.provider.(interface{ Close() error }); isCloser {
			_ = closer.Close()
		}
	}
}

// Lookup 返回已登记服务的 Provider（start/stop 等管理动作用）。
func (r *Registry) Lookup(alias string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[alias]
	if !ok {
		return nil, false
	}
	return e.provider, true
}

// Has 报告 alias 是否已登记。
func (r *Registry) Has(alias string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.entries[alias]
	return ok
}

// Aliases 返回已登记服务（按名排序），是 commands 动态展示的数据源。
func (r *Registry) Aliases() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.entries))
	for alias := range r.entries {
		out = append(out, alias)
	}
	sort.Strings(out)
	return out
}

// StreamHandler 按 RTC DataChannel 标签解析流转发执行体（通道开启时现查，
// 运行期 add 的流式服务无需重启 RTC）。通道开启时过一道调用门；逐消息复查
// 由 Streamer 实现方负责。
func (r *Registry) StreamHandler(label string) (StreamRelay, bool) {
	r.mu.RLock()
	alias, ok := r.channels[label]
	var s Streamer
	if ok {
		// channels 与 entries 同一把写锁维护（不变量）；判空是防御
		if e := r.entries[alias]; e != nil {
			s = e.streamer
		}
	}
	r.mu.RUnlock()
	if !ok || s == nil {
		return nil, false
	}
	return func(ctx context.Context, caller protocol.Caller, input <-chan []byte, send func([]byte) error) error {
		if r.gate != nil {
			// 流通道开门：会话键取 caller.Origin（peer ctx 不携带会话）
			if err := r.gate(ctx, caller.Origin, alias); err != nil {
				return err
			}
		}
		return s.Stream(ctx, caller, input, send)
	}, true
}
