package cua

// cli.go 是 cua 包 CLI 的命令面（v6 P6，原 pod 内建 vsh 指令面随拆包改造为
// 包内 CLI）：process provider（cli/bin/cua）经 skillproc 把 argv/cwd 全量
// 转发给 svc provider，svc 调 Run 执行。输出契约不变：stdout 只放约定 JSON
// （--json 紧凑单行，默认缩进），提示/诊断写 stderr；退出码非零不能当成功
// 数据使用。activate、--delivery foreground 不需要审批——是否允许由 rules
// 决定（命令规则门在 pod 引擎装配侧，包根命令走 execAllowed 门径）。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/veypi/aic-pod/libs/cliargs"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
)

// cuaSub 是一个子命令的声明。
type cuaSub struct {
	usage       string
	positionals []string
	newArgs     func() any
	run         func(ctx context.Context, a any) (any, error)
	special     func(ctx context.Context, args []string) (any, error)
}

func (s *Service) subcommands() []cuaSub {
	subs := []cuaSub{
		{usage: "status", newArgs: func() any { return &Empty{} }, run: func(ctx context.Context, a any) (any, error) {
			return s.Status(ctx, Empty{})
		}},
		{usage: "app.list", newArgs: func() any { return &Empty{} }, run: func(ctx context.Context, a any) (any, error) {
			return s.execute(ctx, "apps", "", nil, map[string]any{}, "none", "")
		}},
		{usage: "app.open <app>", positionals: []string{"app"}, newArgs: func() any { return &OpenArgs{} }, run: func(ctx context.Context, a any) (any, error) {
			return s.execute(ctx, "open", "", nil, fields(a.(*OpenArgs)), "none", "")
		}},
		{usage: "window.list [--app A] [--pid N]", newArgs: func() any { return &ListArgs{} }, run: func(ctx context.Context, a any) (any, error) {
			return s.execute(ctx, "target.list", "", nil, fields(a.(*ListArgs)), "none", "")
		}},
		{usage: "window.observe <window_id> [--query Q] [--depth N] [--image]", positionals: []string{"window_id"}, newArgs: func() any { return &ObserveArgs{} }, run: func(ctx context.Context, a any) (any, error) {
			args := a.(*ObserveArgs)
			if !wire.ValidID(args.WindowID) || args.Depth < 0 || args.Depth > 100 {
				return nil, wire.Fail("invalid_argument", "Invalid window or depth")
			}
			return s.execute(ctx, "snapshot", args.WindowID, nil, fields(args), "none", "")
		}},
		// activate 不需审批（§3.1）：是否允许由 rules 决定。
		{usage: "window.activate <window_id>", positionals: []string{"window_id"}, newArgs: func() any { return &WindowArgs{} }, run: func(ctx context.Context, a any) (any, error) {
			return s.execute(ctx, "activate", a.(*WindowArgs).WindowID, nil, map[string]any{}, "none", "")
		}},
		{usage: "window.menu <window_id> --path a,b,c", positionals: []string{"window_id"}, newArgs: func() any { return &MenuArgs{} }, run: func(ctx context.Context, a any) (any, error) {
			args := a.(*MenuArgs)
			if len(args.Path) == 0 || len(args.Path) > 16 {
				return nil, wire.Fail("invalid_argument", "Expected bounded menu path")
			}
			return s.execute(ctx, "menu", args.WindowID, nil, fields(args), "none", "")
		}},
		{usage: "window.bounds <window_id> --x N --y N --width N --height N", positionals: []string{"window_id"}, newArgs: func() any { return &BoundsArgs{} }, run: func(ctx context.Context, a any) (any, error) {
			args := a.(*BoundsArgs)
			if args.Width <= 0 || args.Height <= 0 || args.Width > 16384 || args.Height > 16384 {
				return nil, wire.Fail("invalid_argument", "Invalid bounds")
			}
			return s.execute(ctx, "window.bounds", args.WindowID, nil, fields(args), "none", "")
		}},
		{usage: "clipboard.read", newArgs: func() any { return &Empty{} }, run: func(ctx context.Context, a any) (any, error) {
			return s.execute(ctx, "clipboard.read", "", nil, map[string]any{}, "none", "")
		}},
		{usage: "clipboard.write <text>", positionals: []string{"text"}, newArgs: func() any { return &TextArgs{} }, run: func(ctx context.Context, a any) (any, error) {
			return s.execute(ctx, "clipboard.write", "", nil, fields(a.(*TextArgs)), "none", "")
		}},
		{usage: "cursor.state", newArgs: func() any { return &Empty{} }, run: func(ctx context.Context, a any) (any, error) {
			return s.execute(ctx, "cursor.state", "", nil, map[string]any{}, "none", "")
		}},
		{usage: "cursor.set --enabled|--enabled=false", newArgs: func() any { return &CursorArgs{} }, run: func(ctx context.Context, a any) (any, error) {
			op := "cursor.off"
			if a.(*CursorArgs).Enabled {
				op = "cursor.on"
			}
			return s.execute(ctx, op, "", nil, map[string]any{}, "none", "")
		}},
		{usage: "window.wait <window_id> [--text T | --role R --name N --state S]", special: func(ctx context.Context, args []string) (any, error) {
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
			return s.execute(ctx, "wait", a.WindowID, locator, fields(&a), "none", "")
		}},
		// drag 的 delivery=foreground 不需审批（§3.1），由 rules 决定。
		{usage: "window.drag <window_id> --snapshot S --from x,y --to x,y [--delivery background|foreground]", positionals: []string{"window_id"}, newArgs: func() any { return &DragArgs{} }, run: func(ctx context.Context, a any) (any, error) {
			args := a.(*DragArgs)
			if args.Snapshot == "" || len(args.From) != 2 || len(args.To) != 2 {
				return nil, wire.Fail("invalid_argument", "Drag requires snapshot and two coordinate pairs")
			}
			return s.execute(ctx, "drag", args.WindowID, nil, fields(args), "none", args.Delivery)
		}},
		{usage: "observation.image.read <window_id> <image_id> [--offset N] [--limit N]", positionals: []string{"window_id", "image_id"}, newArgs: func() any { return &ImageArgs{} }, run: func(ctx context.Context, a any) (any, error) {
			return s.Image(ctx, *a.(*ImageArgs))
		}},
		{usage: "observation.image.export <window_id> <image_id> <path>", positionals: []string{"window_id", "image_id", "path"}, newArgs: func() any { return &ExportArgs{} }, run: func(ctx context.Context, a any) (any, error) {
			return s.ExportImage(ctx, *a.(*ExportArgs))
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
			run: func(ctx context.Context, a any) (any, error) {
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
				return s.execute(ctx, op, args.WindowID, fields(&args.Locator), m, args.After, args.Delivery)
			},
		})
	}
	return subs
}

// Help 是 cua CLI 的使用说明（无参数/--help 时打印到 stdout）。
const Help = `usage: cua <subcommand> [args] [--json]

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
  observation.image.export <window_id> <image_id> <path>   完整截图写文件（推荐）

locator flags：--ref R | --role R --name N | --label L | --snapshot S --at x,y。
选项：--text T --key K --value V --button left|right|middle --count 2
      --delivery background|foreground --after none|observation|image
输出：stdout 只放约定 JSON（--json 紧凑）；诊断写 stderr。
activate 与 --delivery foreground 不需要审批（由 rules 决定）。`

// absolutize 把文件参数按调用方 cwd 绝对化（svc 进程 cwd 是包目录，与调用方
// 无关——browser 包同契约）。
func absolutize(parsed any, cwd string) {
	if a, ok := parsed.(*ExportArgs); ok && a.Path != "" && !filepath.IsAbs(a.Path) && cwd != "" {
		a.Path = filepath.Join(cwd, a.Path)
	}
}

// Run 执行一条 cua CLI 命令（argv 不含程序名），返回进程退出码。
// 与 browser.Service.Run 同契约：--json 任意位置生效；子命令表/解析在上面的
// subcommands()。
func (s *Service) Run(ctx context.Context, argv []string, cwd string, stdout, stderr io.Writer) int {
	subs := s.subcommands()
	byName := map[string]cuaSub{}
	for _, sub := range subs {
		byName[strings.Fields(sub.usage)[0]] = sub
	}
	jsonOut := false
	args := make([]string, 0, len(argv))
	for _, a := range argv {
		if a == "--json" {
			jsonOut = true
			continue
		}
		args = append(args, a)
	}
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		fmt.Fprintln(stdout, Help)
		return 0
	}
	sub, ok := byName[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "cua: unknown subcommand %q\n%s\n", args[0], Help)
		return 2
	}
	var v any
	var err error
	if sub.special != nil {
		v, err = sub.special(ctx, args[1:])
	} else {
		parsed := sub.newArgs()
		if err = cliargs.Parse(args[1:], sub.positionals, parsed); err == nil {
			absolutize(parsed, cwd)
			if validator, ok := parsed.(interface{ Validate() error }); ok {
				err = validator.Validate()
			}
		}
		if err == nil {
			v, err = sub.run(ctx, parsed)
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "cua %s: %s\n", args[0], err)
		return 1
	}
	var raw []byte
	if jsonOut {
		raw, err = json.Marshal(v)
	} else {
		raw, err = json.MarshalIndent(v, "", "  ")
	}
	if err != nil {
		fmt.Fprintf(stderr, "cua %s: encode result: %s\n", args[0], err)
		return 1
	}
	fmt.Fprintln(stdout, string(raw))
	return 0
}
