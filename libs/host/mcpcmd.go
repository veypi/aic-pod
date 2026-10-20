package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/veypi/aic-pod/libs/execution"
	"github.com/veypi/aic-pod/libs/mcpx"
	"github.com/veypi/aic-pod/libs/vtools"
	"github.com/veypi/vsh/commands"
)

// mcp 命令 = 服务管理面（docs/host-vtools.md）：工具调用不再经它中转——
// 服务登记后直接 <alias> <tool> [flags]。tools/describe/call/read 保留为
// 调试直通：tools/describe 透传上游 schema，call 复用 vtools 参数面。

const mcpManageHelp = `usage:
  mcp list [--json]                       服务清单（配置态 + 运行态）
  mcp add <alias> [--cwd DIR] [--env K=V]... [--idle DUR] [--persist] <command> [args...]
  mcp add <alias> --url <http(s)-url> [--persist]
  mcp add <alias> --tcp <host:port> [--token STR] [--persist]  （vtools/tcp 行分隔 JSON 协议）
  mcp remove <alias> [--persist]
  mcp start <alias>                       预连接（登记在 add/启动时已完成；连接本来懒惰）
  mcp stop <alias>                        断开当前连接（下次调用自动重连）
  mcp restart <alias>                     stop + start
  mcp tools|describe|call|read ...        调试直通（原 mcp 命令）

工具调用不再经 mcp 中转：服务登记为 vsh 根命令——<alias> <tool> [--flag value]...
（<alias> 无参列工具目录，<alias> <tool> --help 看参数表）。
add/remove 引入新能力/改配置，需要 exec 规则放行：grant cmd mcp。
--persist 写 config.yaml（重启后仍在）；不带则进程级，重启失效。
内置 browser/cua/blender 不可经 remove 移除（要禁用请在配置文件设 disabled: true）。
手改 config.yaml 需重启 pod 生效；运行期增删请用 mcp add/remove（--persist 才落盘）。`

func (c *Client) cmdMCP(ctx context.Context, inv *commands.Invocation) error {
	args := inv.Args
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(inv.Stdout, mcpManageHelp)
		return nil
	}
	// 调试直通：tools/describe 输出上游原始 schema，call 复用 vtools 参数面，
	// read 直读资源。
	switch args[0] {
	case "tools", "describe", "call", "read":
		return c.mcpDebug(ctx, inv)
	}
	op, rest := args[0], args[1:]
	sid := execution.SessionFromContext(ctx)
	switch op {
	case "list":
		return c.mcpList(inv, rest)
	case "add", "remove":
		if !c.execAllowed(sid, "mcp") {
			return commands.Exitf(inv, 126, "mcp %s: denied by exec rules（引入新能力/改配置需 grant cmd mcp）", op)
		}
		if op == "add" {
			return c.mcpAdd(ctx, inv, rest)
		}
		return c.mcpRemove(ctx, inv, rest)
	case "start", "stop", "restart":
		if len(rest) != 1 {
			return commands.Exitf(inv, 2, "usage: mcp %s <alias>", op)
		}
		if err := c.mcpGate(ctx, execution.SessionFromContext(ctx), rest[0]); err != nil {
			return commands.Exitf(inv, 126, "%s", err)
		}
		return c.mcpStartStop(ctx, inv, op, rest[0])
	default:
		return commands.Exitf(inv, 2, "mcp: unknown operation %q\n%s", op, mcpManageHelp)
	}
}

// mcpDebug 是 mcp 调试直通：tools/describe 保留上游 schema 原样，call 走
// vtools 的通用参数面（--input / 逐 flag / --image-preview），read 直读资源。
func (c *Client) mcpDebug(ctx context.Context, inv *commands.Invocation) error {
	args := inv.Args
	if len(args) < 2 {
		return commands.Exitf(inv, 2, "usage: mcp <tools|describe|call|read> <server> ...")
	}
	op, server, rest := args[0], args[1], args[2:]
	session, err := c.mcpSession(ctx, server)
	if err != nil {
		return commands.Exitf(inv, 126, "mcp: %s", err)
	}
	jsonOut, rest := debugJSON(op, rest)
	switch op {
	case "tools":
		if len(rest) != 0 {
			return commands.Exitf(inv, 2, "usage: mcp tools <server> [--json]")
		}
		tools := []*mcp.Tool{}
		for tool, err := range session.Tools(ctx, nil) {
			if err != nil {
				return commands.Exitf(inv, 1, "mcp: %s", err)
			}
			tools = append(tools, tool)
		}
		return vtools.WriteResult(inv, tools, jsonOut)
	case "describe":
		if len(rest) != 1 {
			return commands.Exitf(inv, 2, "usage: mcp describe <server> <tool> [--json]")
		}
		for tool, err := range session.Tools(ctx, nil) {
			if err != nil {
				return commands.Exitf(inv, 1, "mcp: %s", err)
			}
			if tool.Name == rest[0] {
				return vtools.WriteResult(inv, tool, jsonOut)
			}
		}
		return commands.Exitf(inv, 1, "mcp: unknown tool %q", rest[0])
	case "call":
		if len(rest) == 0 {
			return commands.Exitf(inv, 2, "usage: mcp call <server> <tool> [--input JSON|-] [--json] [--image-preview WxH]")
		}
		provider := vtools.MCP(func(context.Context) (*mcp.ClientSession, error) { return session, nil })
		callInv := *inv
		callInv.Args = rest
		return vtools.RunProvider(ctx, &callInv, server, provider)
	case "read":
		if len(rest) != 1 {
			return commands.Exitf(inv, 2, "usage: mcp read <server> <resource-uri> [--json]")
		}
		value, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: rest[0]})
		if err != nil {
			return commands.Exitf(inv, 1, "mcp: %s", err)
		}
		return vtools.WriteResult(inv, value, jsonOut)
	}
	return commands.Exitf(inv, 2, "mcp: unknown debug operation %q", op)
}

// debugJSON 剥离 --json（仅 tools/describe/read 的合法选项；call 交给
// vtools 统一解析）。
func debugJSON(op string, args []string) (bool, []string) {
	jsonOut := false
	rest := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--json" && op != "call" {
			jsonOut = true
			continue
		}
		rest = append(rest, a)
	}
	return jsonOut, rest
}

// ---- list ----

type mcpListRow struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"` // mcp | tcp
	State    string `json:"state"`
	Idle     string `json:"idle_timeout,omitempty"`
	Inflight int    `json:"inflight,omitempty"`
	Err      string `json:"error,omitempty"`
}

func (c *Client) mcpList(inv *commands.Invocation, args []string) error {
	jsonOut := false
	for _, a := range args {
		if a == "--json" {
			jsonOut = true
		} else {
			return commands.Exitf(inv, 2, "usage: mcp list [--json]")
		}
	}
	rows := []mcpListRow{}
	seen := map[string]bool{}
	rejected := c.serviceRejections()
	if manager := c.mcpManager(); manager != nil {
		for _, s := range manager.Status() {
			seen[s.Name] = true
			state := "configured"
			switch {
			case s.Disabled:
				state = "disabled"
			case s.Running:
				state = "running"
			case s.Starting:
				state = "starting"
			}
			errText := s.Err
			// 管理器有条目但命令面登记失败（撞名/地址无效）：rejected 优先
			if rej, bad := rejected[s.Name]; bad {
				state, errText = "rejected", rej.reason
			}
			rows = append(rows, mcpListRow{Name: s.Name, Kind: "mcp", State: state, Idle: s.IdleTimeout, Inflight: s.Inflight, Err: errText})
		}
	}
	// TCP 条目不走连接管理器：已登记即“registered”。kind 按 provider 实际
	// 类型标（manager 失配时不误标 tcp）。
	if c.svc.registry != nil {
		for _, alias := range c.svc.registry.Aliases() {
			if seen[alias] {
				continue
			}
			kind := "tcp"
			if p, ok := c.svc.registry.Lookup(alias); ok {
				if _, isMCP := p.(*vtools.MCPProvider); isMCP {
					kind = "mcp"
				}
			}
			rows = append(rows, mcpListRow{Name: alias, Kind: kind, State: "registered"})
		}
	}
	// 登记失败的条目：配置还在、命令面没有（TCP 形态不进管理器，这里补行；
	// MCP 形态已在上面按 rejected 覆盖）。
	for name, rej := range rejected {
		if seen[name] {
			continue
		}
		rows = append(rows, mcpListRow{Name: name, Kind: rej.kind, State: "rejected", Err: rej.reason})
	}
	// 配置里 disabled 的 TCP 条目：不进管理器也不登记，补行曝光（MCP 形态已由
	// manager.Status 的 Disabled 分支覆盖；否则「禁用了 TCP 服务」在任何清单都不可见）。
	for name, serverCfg := range c.ownerConfigs() {
		if seen[name] || !serverCfg.Disabled || !serverCfg.IsTCP() {
			continue
		}
		if c.svc.registry != nil && c.svc.registry.Has(name) {
			continue // 上面 registered 段已列
		}
		seen[name] = true
		rows = append(rows, mcpListRow{Name: name, Kind: "tcp", State: "disabled"})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	if jsonOut {
		enc := json.NewEncoder(inv.Stdout)
		return enc.Encode(rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(inv.Stdout, "(no services)")
		return nil
	}
	for _, r := range rows {
		line := r.Name + "\t" + r.Kind + "\t" + r.State
		if r.Idle != "" {
			line += "\tidle=" + r.Idle
		}
		if r.Err != "" {
			line += "\terror=" + r.Err
		}
		fmt.Fprintln(inv.Stdout, line)
	}
	return nil
}

// ---- add ----

func (c *Client) mcpAdd(ctx context.Context, inv *commands.Invocation, args []string) error {
	var (
		alias, rawURL, rawTCP, cwd, idle, token string
		env                                     = map[string]string{}
		persist                                 bool
		positional                              []string
	)
	for n := 0; n < len(args); n++ {
		a := args[n]
		value := func() (string, error) {
			if n+1 >= len(args) {
				return "", fmt.Errorf("%s requires a value", a)
			}
			n++
			return args[n], nil
		}
		switch a {
		case "--url":
			v, err := value()
			if err != nil {
				return commands.Exitf(inv, 2, "mcp add: %s", err)
			}
			rawURL = v
		case "--tcp":
			v, err := value()
			if err != nil {
				return commands.Exitf(inv, 2, "mcp add: %s", err)
			}
			rawTCP = v
		case "--cwd":
			v, err := value()
			if err != nil {
				return commands.Exitf(inv, 2, "mcp add: %s", err)
			}
			cwd = v
		case "--idle":
			v, err := value()
			if err != nil {
				return commands.Exitf(inv, 2, "mcp add: %s", err)
			}
			idle = v
		case "--env":
			v, err := value()
			if err != nil {
				return commands.Exitf(inv, 2, "mcp add: %s", err)
			}
			k, val, ok := strings.Cut(v, "=")
			if !ok || k == "" {
				return commands.Exitf(inv, 2, "mcp add: --env requires K=V")
			}
			env[k] = val
		case "--token":
			v, err := value()
			if err != nil {
				return commands.Exitf(inv, 2, "mcp add: %s", err)
			}
			token = v
		case "--persist":
			persist = true
		default:
			if strings.HasPrefix(a, "--") {
				return commands.Exitf(inv, 2, "mcp add: unknown option %s", a)
			}
			positional = append(positional, a)
		}
	}
	if len(positional) == 0 {
		return commands.Exitf(inv, 2, "%s", mcpManageHelp)
	}
	alias = positional[0]
	var config mcpx.Config
	switch {
	case rawTCP != "":
		if rawURL != "" || len(positional) != 1 {
			return commands.Exitf(inv, 2, "mcp add: --tcp 与 --url/command 互斥")
		}
		if idle != "" {
			return commands.Exitf(inv, 2, "mcp add: --idle 对 TCP 服务无效（连接懒惰，无空闲回收）")
		}
		if cwd != "" || len(env) > 0 {
			return commands.Exitf(inv, 2, "mcp add: --cwd/--env 仅对 command 形态有效")
		}
		config = mcpx.Config{URL: "tcp://" + rawTCP, Token: token}
	case rawURL != "":
		if len(positional) != 1 {
			return commands.Exitf(inv, 2, "mcp add: --url 与 command 互斥")
		}
		if cwd != "" || len(env) > 0 {
			return commands.Exitf(inv, 2, "mcp add: --cwd/--env 仅对 command 形态有效")
		}
		if token != "" {
			return commands.Exitf(inv, 2, "mcp add: --token 仅对 --tcp 形态有效")
		}
		config = mcpx.Config{URL: rawURL, IdleTimeout: idle}
	default:
		if len(positional) < 2 {
			return commands.Exitf(inv, 2, "mcp add: 需要 <command>（或 --url/--tcp）")
		}
		if token != "" {
			return commands.Exitf(inv, 2, "mcp add: --token 仅对 --tcp 形态有效")
		}
		config = mcpx.Config{Command: positional[1], Args: positional[2:], Cwd: cwd, Env: env, IdleTimeout: idle}
	}
	if err := config.Validate(alias); err != nil {
		return commands.Exitf(inv, 2, "mcp add: %s", err)
	}
	if c.svc.registry != nil && c.svc.registry.Has(alias) {
		return commands.Exitf(inv, 1, "mcp add: %s 已存在（先 mcp remove）", alias)
	}
	// 运行态生效：MCP 条目进连接管理器，TCP 条目直接登记；两者都注册 vsh 命令。
	var manager *mcpx.Manager
	if !config.IsTCP() {
		manager = c.mcpManager()
		if manager == nil {
			return commands.Exitf(inv, 1, "mcp add: MCP unavailable")
		}
		if err := manager.Add(alias, config); err != nil {
			return commands.Exitf(inv, 1, "mcp add: %s", err)
		}
	}
	if err := c.registerService(alias, config); err != nil {
		if manager != nil {
			_ = manager.Remove(ctx, alias) // 回滚：不留「管理器有、命令面没有」的半登记状态
		}
		return commands.Exitf(inv, 1, "mcp add: %s，已回滚", err)
	}
	if persist {
		if err := c.persistMCPServer(alias, &config); err != nil {
			return commands.Exitf(inv, 1, "mcp add: 已生效但落盘失败: %s", err)
		}
	}
	// owner 同步：ownerConfigured 据此判定「所有者显式配置」（不加会同进程
	// 旧值——add 了内置同名覆盖项后 remove 仍被误判成内置不可移除）。
	c.svc.mu.Lock()
	if c.svc.owner == nil {
		c.svc.owner = map[string]mcpx.Config{}
	}
	c.svc.owner[alias] = config
	c.svc.mu.Unlock()
	fmt.Fprintf(inv.Stdout, "added %s（%s）\n", alias, map[bool]string{true: "persisted", false: "进程级，重启失效"}[persist])
	return nil
}

// ---- remove ----

func (c *Client) mcpRemove(ctx context.Context, inv *commands.Invocation, args []string) error {
	persist := false
	var positional []string
	for _, a := range args {
		if a == "--persist" {
			persist = true
		} else if strings.HasPrefix(a, "--") {
			return commands.Exitf(inv, 2, "mcp remove: unknown option %s", a)
		} else {
			positional = append(positional, a)
		}
	}
	if len(positional) != 1 {
		return commands.Exitf(inv, 2, "usage: mcp remove <alias> [--persist]")
	}
	alias := positional[0]
	if c.isBuiltinPack(alias) && !c.ownerConfigured(alias) {
		return commands.Exitf(inv, 2, "mcp remove: 内置服务 %s 不可移除（要禁用请在配置文件设 disabled: true）", alias)
	}
	manager := c.mcpManager()
	if manager != nil {
		if err := manager.Remove(ctx, alias); err != nil && !errors.Is(err, mcpx.ErrUnknownService) {
			return commands.Exitf(inv, 1, "mcp remove: %s", err)
		}
	}
	if c.svc.registry == nil || !c.svc.registry.Has(alias) {
		// 登记失败的条目（mcp list 的 rejected 行）：摘掉曝光记录；否则是未知服务
		if _, rejected := c.serviceRejections()[alias]; rejected {
			c.clearServiceRejection(alias)
		} else if !persist {
			return commands.Exitf(inv, 1, "mcp remove: unknown service %q", alias)
		}
	} else {
		c.svc.registry.Unregister(alias)
	}
	if persist {
		if err := c.persistMCPServer(alias, nil); err != nil {
			return commands.Exitf(inv, 1, "mcp remove: 已生效但落盘失败: %s", err)
		}
	}
	// 同 add：owner 同步摘除（ownerConfigured 判定源）。
	c.svc.mu.Lock()
	delete(c.svc.owner, alias)
	c.svc.mu.Unlock()
	fmt.Fprintf(inv.Stdout, "removed %s\n", alias)
	return nil
}

// ---- start / stop / restart ----

func (c *Client) mcpStartStop(ctx context.Context, inv *commands.Invocation, op, alias string) error {
	manager := c.mcpManager()
	isMCP := false
	if manager != nil {
		for _, s := range manager.Status() {
			if s.Name == alias {
				isMCP = true
				break
			}
		}
	}
	if !isMCP {
		if c.svc.registry == nil {
			return commands.Exitf(inv, 1, "mcp %s: unknown service %q", op, alias)
		}
		if _, ok := c.svc.registry.Lookup(alias); !ok {
			return commands.Exitf(inv, 1, "mcp %s: unknown service %q", op, alias)
		}
	}
	if op != "start" {
		if isMCP {
			if err := manager.Restart(ctx, alias); err != nil {
				return commands.Exitf(inv, 1, "mcp %s: %s", op, err)
			}
		} else if p, ok := c.svc.registry.Lookup(alias); ok {
			if closer, isCloser := p.(interface{ Close() error }); isCloser {
				if err := closer.Close(); err != nil {
					return commands.Exitf(inv, 1, "mcp %s: %s", op, err)
				}
			}
		}
	}
	if op != "stop" {
		if isMCP {
			if _, err := manager.Session(ctx, alias); err != nil {
				return commands.Exitf(inv, 1, "mcp %s: %s", op, err)
			}
		} else if p, ok := c.svc.registry.Lookup(alias); ok {
			if _, err := p.Tools(ctx); err != nil {
				return commands.Exitf(inv, 1, "mcp %s: %s", op, err)
			}
		}
	}
	fmt.Fprintf(inv.Stdout, "%s %s\n", op, alias)
	return nil
}
