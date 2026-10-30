package host

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/execution"
	"github.com/veypi/vsh/commands"
)

const hostCommandsHelp = `usage:
  commands                              列出 host 当前可发现的命令
  commands list [--json]                列出统一注册的命令
  commands info <alias> [--schema]      查看定义或 MCP inputSchema
  commands install <skill_id>           下载并注册技能设备命令
  commands update <skill_id>            更新技能包并重新注册
  commands uninstall <skill_id>         移除技能包及其命令
  commands config <alias> [K=V ...] [--unset K] [--no-sandbox|--sandbox] [--clear] [--json]
                                        命令定义级配置（应用配置 env + 沙箱策略）
  commands add <alias> [--protocol mcp] [--cwd DIR] [--env K=V]... [--idle DUR] [--persist] -- <program> [args...]
  commands add <alias> --protocol mcp --url URL [--persist]
  commands remove <alias> [--persist]
  commands start|stop|restart <alias>
  commands mcp <alias> tools|describe|call|read ...`

type hostCommandRow struct {
	Name    string       `json:"name"`
	ID      string       `json:"id,omitempty"`
	Kind    string       `json:"kind"`
	Source  string       `json:"source,omitempty"`
	State   string       `json:"state"`
	Error   string       `json:"error,omitempty"`
	Streams []hostStream `json:"streams,omitempty"`
}

func (c *Client) cmdHostCommands(ctx context.Context, inv *commands.Invocation) error {
	args := inv.Args
	if len(args) == 0 {
		if inv.GetRegisteredCommands == nil {
			return commands.Exitf(inv, 1, "commands: registry unavailable")
		}
		for _, name := range inv.GetRegisteredCommands() {
			if c.hostDiscoverable(name) {
				fmt.Fprintln(inv.Stdout, name)
			}
		}
		return nil
	}
	if args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		fmt.Fprintln(inv.Stdout, hostCommandsHelp)
		return nil
	}
	op, rest := args[0], args[1:]
	switch op {
	case "list":
		return c.hostCommandList(inv, rest)
	case "info":
		return c.hostCommandInfo(ctx, inv, rest)
	case "install", "update", "uninstall":
		if !c.execAllowed(execution.SessionFromContext(ctx), "commands") {
			return commands.Exitf(inv, 126, "commands %s: denied; request grant cmd commands", op)
		}
		if len(rest) != 1 {
			return commands.Exitf(inv, 2, "usage: commands %s <skill_id>", op)
		}
		var err error
		if op == "uninstall" {
			err = c.uninstallCommandPackage(rest[0])
		} else {
			err = c.installCommandPackage(ctx, rest[0])
		}
		if err != nil {
			return commands.Exitf(inv, 1, "commands %s: %s", op, err)
		}
		fmt.Fprintf(inv.Stdout, "%s %s\n", op, rest[0])
		return nil
	case "config":
		if !c.execAllowed(execution.SessionFromContext(ctx), "commands") {
			return commands.Exitf(inv, 126, "commands config: denied; request grant cmd commands")
		}
		return c.hostCommandConfig(ctx, inv, rest)
	case "add", "remove":
		if !c.execAllowed(execution.SessionFromContext(ctx), "commands") {
			return commands.Exitf(inv, 126, "commands %s: denied; request grant cmd commands", op)
		}
		if op == "add" {
			return c.hostCommandAdd(inv, rest)
		}
		return c.hostCommandRemove(inv, rest)
	case "start", "stop", "restart":
		if len(rest) != 1 {
			return commands.Exitf(inv, 2, "usage: commands %s <alias>", op)
		}
		if !c.execAllowed(execution.SessionFromContext(ctx), rest[0]) {
			return commands.Exitf(inv, 126, "commands %s: denied; request grant cmd %s", op, rest[0])
		}
		return c.cmds.startStop(ctx, inv, op, rest[0])
	case "mcp":
		if len(rest) < 2 {
			return commands.Exitf(inv, 2, "usage: commands mcp <alias> <tools|describe|call|read> ...")
		}
		if !c.execAllowed(execution.SessionFromContext(ctx), rest[0]) {
			return commands.Exitf(inv, 126, "commands mcp: denied; request grant cmd %s", rest[0])
		}
		debugInv := *inv
		debugInv.Args = append([]string{rest[1], rest[0]}, rest[2:]...)
		return c.mcpDebug(ctx, &debugInv)
	default:
		return commands.Exitf(inv, 2, "commands: unknown operation %q\n%s", op, hostCommandsHelp)
	}
}

func (c *Client) hostCommandRows() []hostCommandRow {
	c.cmds.mu.RLock()
	defer c.cmds.mu.RUnlock()
	rows := make([]hostCommandRow, 0, len(c.cmds.active)+len(c.cmds.rejected))
	for alias, def := range c.cmds.active {
		row := hostCommandRow{Name: alias, ID: def.ID, Kind: def.kind(), Source: def.Source, State: "ready"}
		if def.Config.Protocol == "mcp" && c.cmds.manager != nil {
			for _, status := range c.cmds.manager.Status() {
				if status.Name == alias {
					row.State = "configured"
					if status.Running {
						row.State = "running"
					} else if status.Starting {
						row.State = "starting"
					}
					row.Error = status.Err
				}
			}
		}
		if process := c.cmds.processes[alias]; process != nil && process.isRunning() {
			row.State = "running"
		}
		for _, stream := range def.Streams {
			row.Streams = append(row.Streams, stream)
		}
		sort.Slice(row.Streams, func(i, j int) bool { return row.Streams[i].Name < row.Streams[j].Name })
		rows = append(rows, row)
	}
	for alias, reason := range c.cmds.rejected {
		rows = append(rows, hostCommandRow{Name: alias, Kind: "unknown", State: "rejected", Error: reason})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows
}

func (c *Client) hostCommandList(inv *commands.Invocation, args []string) error {
	jsonOut := false
	for _, arg := range args {
		if arg == "--json" {
			jsonOut = true
		} else {
			return commands.Exitf(inv, 2, "usage: commands list [--json]")
		}
	}
	rows := c.hostCommandRows()
	if jsonOut {
		return json.NewEncoder(inv.Stdout).Encode(rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(inv.Stdout, "(no registered commands)")
		return nil
	}
	for _, row := range rows {
		line := row.Name + "\t" + row.Kind + "\t" + row.State
		if row.Source != "" {
			line += "\t" + row.Source
		}
		if row.Error != "" {
			line += "\terror=" + row.Error
		}
		fmt.Fprintln(inv.Stdout, line)
	}
	return nil
}

func (c *Client) hostCommandInfo(ctx context.Context, inv *commands.Invocation, args []string) error {
	schema := false
	var positional []string
	for _, arg := range args {
		if arg == "--schema" {
			schema = true
		} else {
			positional = append(positional, arg)
		}
	}
	if len(positional) != 1 {
		return commands.Exitf(inv, 2, "usage: commands info <alias> [--schema]")
	}
	alias := positional[0]
	if schema {
		session, err := c.cmds.session(ctx, alias)
		if err != nil {
			return commands.Exitf(inv, 1, "commands info: %s", err)
		}
		var tools []any
		for tool, err := range session.Tools(ctx, nil) {
			if err != nil {
				return commands.Exitf(inv, 1, "commands info: %s", err)
			}
			tools = append(tools, tool)
		}
		return json.NewEncoder(inv.Stdout).Encode(tools)
	}
	for _, row := range c.hostCommandRows() {
		if row.Name == alias {
			enc := json.NewEncoder(inv.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(row)
		}
	}
	return commands.Exitf(inv, 1, "commands info: unknown command %q", alias)
}

func (c *Client) hostCommandAdd(inv *commands.Invocation, args []string) error {
	var alias, rawURL, protocol, cwd, idle string
	env := map[string]string{}
	persist := false
	var run []string
	for n := 0; n < len(args); n++ {
		arg := args[n]
		if arg == "--" {
			run = append([]string(nil), args[n+1:]...)
			break
		}
		value := func() (string, error) {
			if n+1 >= len(args) {
				return "", fmt.Errorf("%s requires a value", arg)
			}
			n++
			return args[n], nil
		}
		switch arg {
		case "--url":
			v, err := value()
			if err != nil {
				return commands.Exitf(inv, 2, "commands add: %s", err)
			}
			rawURL = v
		case "--protocol":
			v, err := value()
			if err != nil {
				return commands.Exitf(inv, 2, "commands add: %s", err)
			}
			protocol = v
		case "--cwd":
			v, err := value()
			if err != nil {
				return commands.Exitf(inv, 2, "commands add: %s", err)
			}
			cwd = v
		case "--idle":
			v, err := value()
			if err != nil {
				return commands.Exitf(inv, 2, "commands add: %s", err)
			}
			idle = v
		case "--env":
			v, err := value()
			if err != nil {
				return commands.Exitf(inv, 2, "commands add: %s", err)
			}
			key, val, ok := strings.Cut(v, "=")
			if !ok || key == "" {
				return commands.Exitf(inv, 2, "commands add: --env requires K=V")
			}
			env[key] = val
		case "--persist":
			persist = true
		default:
			if strings.HasPrefix(arg, "--") {
				return commands.Exitf(inv, 2, "commands add: unknown option %s", arg)
			}
			if alias != "" {
				return commands.Exitf(inv, 2, "commands add: alias must precede --")
			}
			alias = arg
		}
	}
	if alias == "" || rawURL == "" && len(run) == 0 {
		return commands.Exitf(inv, 2, "%s", hostCommandsHelp)
	}
	config := cfg.CommandConfig{Run: run, URL: rawURL, Protocol: protocol, Cwd: cwd, Env: env, IdleTimeout: idle}
	def := &hostCommand{ID: "config:" + alias, Alias: alias, Source: commandSourceConfig, Config: config}
	if err := validateHostCommand(def); err != nil {
		return commands.Exitf(inv, 2, "commands add: %s", err)
	}
	if persist {
		if err := c.persistCommandConfig(alias, &config); err != nil {
			return commands.Exitf(inv, 1, "commands add: %s", err)
		}
		if err := c.cmds.reload(); err != nil {
			return commands.Exitf(inv, 1, "commands add: persisted but reload failed: %s", err)
		}
	} else if err := c.cmds.upsertRuntime(def); err != nil {
		return commands.Exitf(inv, 1, "commands add: %s", err)
	}
	fmt.Fprintf(inv.Stdout, "added %s\n", alias)
	return nil
}

func (c *Client) hostCommandRemove(inv *commands.Invocation, args []string) error {
	persist := false
	var positional []string
	for _, arg := range args {
		if arg == "--persist" {
			persist = true
		} else {
			positional = append(positional, arg)
		}
	}
	if len(positional) != 1 {
		return commands.Exitf(inv, 2, "usage: commands remove <alias> [--persist]")
	}
	alias := positional[0]
	id := "config:" + alias
	_, runtime := c.cmds.runtime[id]
	_, file := c.options().Commands[alias]
	if persist {
		c.cmds.rebuildMu.Lock()
		delete(c.cmds.runtime, id)
		c.cmds.rebuildMu.Unlock()
		if err := c.persistCommandConfig(alias, nil); err != nil {
			return commands.Exitf(inv, 1, "commands remove: %s", err)
		}
		if err := c.cmds.reload(); err != nil {
			return commands.Exitf(inv, 1, "commands remove: %s", err)
		}
	} else {
		if !runtime {
			if file {
				return commands.Exitf(inv, 2, "commands remove: %s is persisted; use --persist", alias)
			}
			return commands.Exitf(inv, 1, "commands remove: unknown manual command %q", alias)
		}
		if err := c.cmds.removeRuntime(id); err != nil {
			return commands.Exitf(inv, 1, "commands remove: %s", err)
		}
	}
	fmt.Fprintf(inv.Stdout, "removed %s\n", alias)
	return nil
}

func (p *streamProcess) isRunning() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running
}
