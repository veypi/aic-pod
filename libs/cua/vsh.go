package cua

// vsh.go 是 cua 的 vsh 指令面（hosts-vsh-redesign §2/§3.1/§4.5）：
// cua 直接注册为 vsh 指令，与其他 shell 指令自由组合。Browser/CUA 不需要
// 审批——activate、--delivery foreground 也不例外；是否允许由 rules 决定
// （命令规则门在引擎装配侧）。输出契约：stdout 只放约定 JSON（--json 紧凑），
// 诊断写 stderr。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/veypi/aic-pod/libs/cliargs"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
	"github.com/veypi/vsh/commands"
)

// cuaSub 是一个子命令的声明。
type cuaSub struct {
	usage       string
	positionals []string
	newArgs     func() any
	run         func(ctx context.Context, c wire.Caller, a any) (any, error)
	special     func(ctx context.Context, c wire.Caller, args []string) (any, error)
}

func (s *Service) subcommands() []cuaSub {
	subs := []cuaSub{
		{usage: "status", newArgs: func() any { return &Empty{} }, run: func(ctx context.Context, c wire.Caller, a any) (any, error) {
			return s.Status(ctx, c, Empty{})
		}},
		{usage: "app.list", newArgs: func() any { return &Empty{} }, run: func(ctx context.Context, c wire.Caller, a any) (any, error) {
			return s.execute(ctx, c, "apps", "", nil, map[string]any{}, "none", "")
		}},
		{usage: "app.open <app>", positionals: []string{"app"}, newArgs: func() any { return &OpenArgs{} }, run: func(ctx context.Context, c wire.Caller, a any) (any, error) {
			return s.execute(ctx, c, "open", "", nil, fields(a.(*OpenArgs)), "none", "")
		}},
		{usage: "window.list [--app A] [--pid N]", newArgs: func() any { return &ListArgs{} }, run: func(ctx context.Context, c wire.Caller, a any) (any, error) {
			return s.execute(ctx, c, "target.list", "", nil, fields(a.(*ListArgs)), "none", "")
		}},
		{usage: "window.observe <window_id> [--query Q] [--depth N] [--image]", positionals: []string{"window_id"}, newArgs: func() any { return &ObserveArgs{} }, run: func(ctx context.Context, c wire.Caller, a any) (any, error) {
			args := a.(*ObserveArgs)
			if !wire.ValidID(args.WindowID) || args.Depth < 0 || args.Depth > 100 {
				return nil, wire.Fail("invalid_argument", "Invalid window or depth")
			}
			return s.execute(ctx, c, "snapshot", args.WindowID, nil, fields(args), "none", "")
		}},
		// activate 不需审批（§3.1）：是否允许由 rules 决定。
		{usage: "window.activate <window_id>", positionals: []string{"window_id"}, newArgs: func() any { return &WindowArgs{} }, run: func(ctx context.Context, c wire.Caller, a any) (any, error) {
			return s.execute(ctx, c, "activate", a.(*WindowArgs).WindowID, nil, map[string]any{}, "none", "")
		}},
		{usage: "window.menu <window_id> --path a,b,c", positionals: []string{"window_id"}, newArgs: func() any { return &MenuArgs{} }, run: func(ctx context.Context, c wire.Caller, a any) (any, error) {
			args := a.(*MenuArgs)
			if len(args.Path) == 0 || len(args.Path) > 16 {
				return nil, wire.Fail("invalid_argument", "Expected bounded menu path")
			}
			return s.execute(ctx, c, "menu", args.WindowID, nil, fields(args), "none", "")
		}},
		{usage: "window.bounds <window_id> --x N --y N --width N --height N", positionals: []string{"window_id"}, newArgs: func() any { return &BoundsArgs{} }, run: func(ctx context.Context, c wire.Caller, a any) (any, error) {
			args := a.(*BoundsArgs)
			if args.Width <= 0 || args.Height <= 0 || args.Width > 16384 || args.Height > 16384 {
				return nil, wire.Fail("invalid_argument", "Invalid bounds")
			}
			return s.execute(ctx, c, "window.bounds", args.WindowID, nil, fields(args), "none", "")
		}},
		{usage: "clipboard.read", newArgs: func() any { return &Empty{} }, run: func(ctx context.Context, c wire.Caller, a any) (any, error) {
			return s.execute(ctx, c, "clipboard.read", "", nil, map[string]any{}, "none", "")
		}},
		{usage: "clipboard.write <text>", positionals: []string{"text"}, newArgs: func() any { return &TextArgs{} }, run: func(ctx context.Context, c wire.Caller, a any) (any, error) {
			return s.execute(ctx, c, "clipboard.write", "", nil, fields(a.(*TextArgs)), "none", "")
		}},
		{usage: "cursor.state", newArgs: func() any { return &Empty{} }, run: func(ctx context.Context, c wire.Caller, a any) (any, error) {
			return s.execute(ctx, c, "cursor.state", "", nil, map[string]any{}, "none", "")
		}},
		{usage: "cursor.set --enabled|--enabled=false", newArgs: func() any { return &CursorArgs{} }, run: func(ctx context.Context, c wire.Caller, a any) (any, error) {
			op := "cursor.off"
			if a.(*CursorArgs).Enabled {
				op = "cursor.on"
			}
			return s.execute(ctx, c, op, "", nil, map[string]any{}, "none", "")
		}},
		{usage: "window.wait <window_id> [--text T | --role R --name N --state S]", special: func(ctx context.Context, c wire.Caller, args []string) (any, error) {
			var flat struct {
				WindowID string `json:"window_id"`
				Text     string `json:"text"`
				State    string `json:"state"`
				Locator  Locator
			}
			if err := cliargs.Parse(args, []string{"window_id"}, &flat); err != nil {
				return nil, err
			}
			a := WaitArgs{WindowID: flat.WindowID, State: flat.State}
			if flat.Text != "" {
				a.Text = &flat.Text
			}
			if flat.Locator.Ref != "" || flat.Locator.Role != "" || flat.Locator.Label != "" || len(flat.Locator.At) > 0 {
				a.Locator = &flat.Locator
			}
			if (a.Locator == nil) == (a.Text == nil) {
				return nil, wire.Fail("invalid_argument", "Provide text or semantic locator")
			}
			if a.Locator != nil {
				if err := a.Locator.Validate(); err != nil {
					return nil, err
				}
				if a.Locator.Ref != "" || len(a.Locator.At) > 0 || a.State == "" {
					return nil, wire.Fail("invalid_argument", "Wait requires semantic locator and state")
				}
			}
			var locator map[string]any
			if a.Locator != nil {
				locator = fields(a.Locator)
			}
			return s.execute(ctx, c, "wait", a.WindowID, locator, fields(&a), "none", "")
		}},
		// drag 的 delivery=foreground 不需审批（§3.1），由 rules 决定。
		{usage: "window.drag <window_id> --snapshot S --from x,y --to x,y [--delivery background|foreground]", positionals: []string{"window_id"}, newArgs: func() any { return &DragArgs{} }, run: func(ctx context.Context, c wire.Caller, a any) (any, error) {
			args := a.(*DragArgs)
			if args.Snapshot == "" || len(args.From) != 2 || len(args.To) != 2 {
				return nil, wire.Fail("invalid_argument", "Drag requires snapshot and two coordinate pairs")
			}
			return s.execute(ctx, c, "drag", args.WindowID, nil, fields(args), "none", args.Delivery)
		}},
		{usage: "observation.image.read <window_id> <image_id> [--offset N] [--limit N]", positionals: []string{"window_id", "image_id"}, newArgs: func() any { return &ImageArgs{} }, run: func(ctx context.Context, c wire.Caller, a any) (any, error) {
			return s.Image(ctx, c, *a.(*ImageArgs))
		}},
	}
	// 窗口动作（click/fill/type/press/scroll/set/move）同形：locator flags
	//（--ref/--role+--name/--label/--snapshot+--at x,y）+ --text/--key/--value/
	// --button/--count/--delivery/--after。delivery=foreground 不需审批（§3.1）。
	for _, name := range []string{"click", "fill", "type", "press", "scroll", "set", "move"} {
		op := name
		subs = append(subs, cuaSub{
			usage:       "window." + op + " <window_id> <--ref|--role+--name|--label|--snapshot+--at x,y> [选项]",
			positionals: []string{"window_id"},
			newArgs:     func() any { return &ActionArgs{} },
			run: func(ctx context.Context, c wire.Caller, a any) (any, error) {
				args := a.(*ActionArgs)
				if !wire.ValidID(args.WindowID) || args.Count < 0 || args.Count > 2 {
					return nil, wire.Fail("invalid_argument", "Invalid window or click count")
				}
				if err := args.Locator.Validate(); err != nil {
					return nil, err
				}
				m := fields(args)
				if args.Count == 2 {
					m["count"] = "2"
				}
				return s.execute(ctx, c, op, args.WindowID, fields(&args.Locator), m, args.After, args.Delivery)
			},
		})
	}
	return subs
}

const cuaHelp = `usage: cua <subcommand> [args] [--json]

子命令：
  status                      驱动状态
  app.list                    列出应用
  app.open <app>              打开应用
  window.list [--app A] [--pid N]
  window.observe <window_id> [--query Q] [--depth N] [--image]
  window.activate <window_id>
  window.menu <window_id> --path a,b,c
  window.bounds <window_id> --x N --y N --width N --height N
  window.wait <window_id> [--text T | --role R --name N --state S]
  window.<click|fill|type|press|scroll|set|move> <window_id> <locator-flags> [选项]
  window.drag <window_id> --snapshot S --from x,y --to x,y [--delivery ...]
  clipboard.read | clipboard.write <text>
  cursor.state | cursor.set --enabled[=false]
  observation.image.read <window_id> <image_id> [--offset N] [--limit N]

locator flags：--ref R | --role R --name N | --label L | --snapshot S --at x,y。
选项：--text T --key K --value V --button left|right|middle --count 2
      --delivery background|foreground --after none|observation|image
输出：stdout 只放约定 JSON（--json 紧凑）；诊断写 stderr。
activate 与 --delivery foreground 不需要审批（由 rules 决定）。`

// VshCommand 构造 cua 的 vsh 指令。callerOf 由执行上下文构造业务身份
// （host 装配注入 vshglue.CallerFromContext——cua 不反向依赖 vsh glue）。
func (s *Service) VshCommand(callerOf func(context.Context) wire.Caller) commands.Command {
	subs := s.subcommands()
	byName := map[string]cuaSub{}
	for _, sub := range subs {
		byName[strings.Fields(sub.usage)[0]] = sub
	}
	return commands.DefineCommand("cua", func(ctx context.Context, inv *commands.Invocation) error {
		jsonOut := false
		args := make([]string, 0, len(inv.Args))
		for _, a := range inv.Args {
			if a == "--json" {
				jsonOut = true
				continue
			}
			args = append(args, a)
		}
		if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
			fmt.Fprintln(inv.Stdout, cuaHelp)
			return nil
		}
		sub, ok := byName[args[0]]
		if !ok {
			return commands.Exitf(inv, 2, "cua: unknown subcommand %q\n%s", args[0], cuaHelp)
		}
		caller := callerOf(ctx)
		var v any
		var err error
		if sub.special != nil {
			v, err = sub.special(ctx, caller, args[1:])
		} else {
			parsed := sub.newArgs()
			if err = cliargs.Parse(args[1:], sub.positionals, parsed); err == nil {
				if validator, ok := parsed.(interface{ Validate() error }); ok {
					err = validator.Validate()
				}
			}
			if err == nil {
				v, err = sub.run(ctx, caller, parsed)
			}
		}
		if err != nil {
			return commands.Exitf(inv, 1, "cua %s: %s", args[0], err)
		}
		var raw []byte
		if jsonOut {
			raw, err = json.Marshal(v)
		} else {
			raw, err = json.MarshalIndent(v, "", "  ")
		}
		if err != nil {
			return commands.Exitf(inv, 1, "cua %s: encode result: %s", args[0], err)
		}
		fmt.Fprintln(inv.Stdout, string(raw))
		return nil
	})
}
