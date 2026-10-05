package mcpx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/veypi/vsh/commands"
)

type Resolve func(context.Context, string) (*mcp.ClientSession, error)

const commandHelp = `usage:
  mcp tools <server>
  mcp describe <server> <tool>
  mcp call <server> <tool> [--input JSON|-] [--json]
  mcp read <server> <resource-uri> [--json]

Services are configured on the execution host selected by exec.1host. Paths are explicit tool arguments;
the current shell directory and environment do not change a shared service.`

func Command(resolve Resolve) commands.CommandFunc {
	return func(ctx context.Context, inv *commands.Invocation) error {
		args := inv.Args
		if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
			fmt.Fprintln(inv.Stdout, commandHelp)
			return nil
		}
		input, jsonOut := "{}", false
		var positional []string
		for n := 0; n < len(args); n++ {
			switch args[n] {
			case "--input":
				if n+1 >= len(args) {
					return commands.Exitf(inv, 2, "mcp: %s requires a value", args[n])
				}
				input = args[n+1]
				n++
			case "--json":
				jsonOut = true
			default:
				if strings.HasPrefix(args[n], "-") {
					return commands.Exitf(inv, 2, "mcp: unknown option %s", args[n])
				}
				positional = append(positional, args[n])
			}
		}
		if len(positional) < 2 {
			return commands.Exitf(inv, 2, "%s", commandHelp)
		}
		op, server := positional[0], positional[1]
		if (op == "tools" && len(positional) != 2) || (op != "tools" && len(positional) != 3) {
			return commands.Exitf(inv, 2, "%s", commandHelp)
		}
		if op != "tools" && op != "describe" && op != "call" && op != "read" {
			return commands.Exitf(inv, 2, "mcp: unknown operation %q", op)
		}
		session, err := resolve(ctx, server)
		if err != nil {
			return commands.Exitf(inv, 126, "mcp: %s", err)
		}
		var value any
		failed := false
		switch op {
		case "tools", "describe":
			list := []*mcp.Tool{}
			for tool, e := range session.Tools(ctx, nil) {
				if e != nil {
					err = e
					break
				}
				if op == "describe" {
					if tool.Name == positional[2] {
						value = tool
						break
					}
				} else {
					list = append(list, tool)
				}
			}
			if op == "tools" {
				value = list
			} else if err == nil && value == nil {
				err = fmt.Errorf("unknown tool %q", positional[2])
			}
		case "call":
			var data []byte
			if input == "-" {
				if inv.Stdin == nil {
					err = fmt.Errorf("stdin is unavailable")
				} else {
					data, err = io.ReadAll(io.LimitReader(inv.Stdin, 1<<20+1))
				}
			} else {
				data = []byte(input)
			}
			if err == nil && len(data) > 1<<20 {
				err = fmt.Errorf("input exceeds 1 MiB")
			}
			var params map[string]any
			if err == nil {
				err = json.Unmarshal(data, &params)
				if params == nil && err == nil {
					err = fmt.Errorf("input must be a JSON object")
				}
			}
			if err == nil {
				var result *mcp.CallToolResult
				result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: positional[2], Arguments: params})
				if result != nil {
					value = result
					failed = result.IsError
					if result.NeedsInput() {
						err = fmt.Errorf("tool requires unsupported interactive input")
					}
				}
			}
		case "read":
			value, err = session.ReadResource(ctx, &mcp.ReadResourceParams{URI: positional[2]})
		}
		if err != nil {
			return commands.Exitf(inv, 1, "mcp: %s", err)
		}
		enc := json.NewEncoder(inv.Stdout)
		if !jsonOut {
			enc.SetIndent("", "  ")
		}
		if err = enc.Encode(value); err != nil {
			if commands.BrokenPipe(err) {
				// 下游提前关闭（`mcp ... | head`）：与 vsh 内建命令（cat/printf/tee…）
				// 同一约定，按 SIGPIPE 语义返回 141。裸 error 会被解释器当作致命中止，
				// 会把常用的截断管道变成整段脚本中断。
				return &commands.ExitError{Code: 141}
			}
			return err
		}
		if failed {
			return &commands.ExitError{Code: 1}
		}
		return nil
	}
}
