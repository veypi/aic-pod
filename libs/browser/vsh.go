package browser

// vsh.go 是 browser 的 vsh 指令面（hosts-vsh-redesign §2/§4.5）：
// browser 直接注册为 vsh 指令，与其他 shell 指令自由组合（管道/重定向/
// 条件/循环同语义）。输出契约：stdout 只放约定 JSON（--json 紧凑单行，
// 默认缩进），提示/诊断/警告写 stderr；非零退出不能当成功数据使用。
//
// page.frames / page.input 不在此——它们是 RTC 私有 stream 端点（§4.3）。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/veypi/aic-pod/libs/cliargs"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
	"github.com/veypi/vsh/commands"
)

// browserSub 是一个子命令的声明。
type browserSub struct {
	usage       string
	aliases     []string
	positionals []string
	newArgs     func() any
	run         func(ctx context.Context, c wire.Caller, a any) (any, error)
	// special 接管原始参数（evaluate 等整段文本参数）。
	special func(ctx context.Context, c wire.Caller, args []string) (any, error)
}

func (s *Service) subcommands() []browserSub {
	subs := []browserSub{
		{usage: "status", newArgs: func() any { return &Empty{} }, run: func(ctx context.Context, c wire.Caller, a any) (any, error) {
			return s.Status(ctx, c, Empty{})
		}},
		{usage: "page.list", aliases: []string{"pages"}, newArgs: func() any { return &Empty{} }, run: func(ctx context.Context, c wire.Caller, a any) (any, error) {
			return s.List(ctx, c, Empty{})
		}},
		{usage: "page.create [url] [--width N] [--height N]", aliases: []string{"open"}, positionals: []string{"url?"}, newArgs: func() any { return &CreateArgs{} }, run: func(ctx context.Context, c wire.Caller, a any) (any, error) {
			return s.Create(ctx, c, *a.(*CreateArgs))
		}},
		{usage: "page.navigate <page_id> <url>", aliases: []string{"navigate"}, positionals: []string{"page_id", "url"}, newArgs: func() any { return &NavigateArgs{} }, run: func(ctx context.Context, c wire.Caller, a any) (any, error) {
			return s.Navigate(ctx, c, *a.(*NavigateArgs))
		}},
		{usage: "page.close <page_id>", aliases: []string{"close"}, positionals: []string{"page_id"}, newArgs: func() any { return &PageArgs{} }, run: func(ctx context.Context, c wire.Caller, a any) (any, error) {
			return s.ClosePage(ctx, c, *a.(*PageArgs))
		}},
		{usage: "page.observe <page_id> [--query Q] [--limit N] [--image]", aliases: []string{"observe"}, positionals: []string{"page_id"}, newArgs: func() any { return &ObserveArgs{} }, run: func(ctx context.Context, c wire.Caller, a any) (any, error) {
			return s.Observe(ctx, c, *a.(*ObserveArgs))
		}},
		{usage: "page.wait <page_id> [--text T|--url U|--load|--ref/--css/--role+--name/--label + --state S] [--timeout_ms N]", aliases: []string{"wait"}, special: func(ctx context.Context, c wire.Caller, args []string) (any, error) {
			// WaitArgs.Locator 是指针——定位旗标单独收拢后构造。
			var flat struct {
				PageID    string `json:"page_id"`
				Text      string `json:"text"`
				URL       string `json:"url"`
				Load      bool   `json:"load"`
				State     string `json:"state"`
				TimeoutMS int64  `json:"timeout_ms"`
				Locator   Locator
			}
			if err := cliargs.Parse(args, []string{"page_id"}, &flat); err != nil {
				return nil, err
			}
			a := WaitArgs{PageID: flat.PageID, Text: flat.Text, URL: flat.URL, Load: flat.Load, State: flat.State, TimeoutMS: flat.TimeoutMS}
			if flat.Locator != (Locator{}) {
				a.Locator = &flat.Locator
			}
			if err := a.Validate(); err != nil {
				return nil, err
			}
			return s.Wait(ctx, c, a)
		}},
		{usage: "page.events <page_id> [--cursor N] [--kind K]", aliases: []string{"events"}, positionals: []string{"page_id"}, newArgs: func() any { return &EventsArgs{} }, run: func(ctx context.Context, c wire.Caller, a any) (any, error) {
			return s.Events(ctx, c, *a.(*EventsArgs))
		}},
		{usage: "page.dialog.resolve <page_id> <dialog_id> [--accept|--accept=false] [--text T]", aliases: []string{"dialog"}, positionals: []string{"page_id", "dialog_id"}, newArgs: func() any { return &DialogArgs{} }, run: func(ctx context.Context, c wire.Caller, a any) (any, error) {
			return s.Dialog(ctx, c, *a.(*DialogArgs))
		}},
		{usage: "page.evaluate <page_id> <code...>", aliases: []string{"eval"}, special: func(ctx context.Context, c wire.Caller, args []string) (any, error) {
			if len(args) < 2 {
				return nil, wire.Fail("invalid_argument", "usage: page.evaluate <page_id> <code...>")
			}
			return s.Evaluate(ctx, c, EvaluateArgs{PageID: args[0], Code: strings.Join(args[1:], " ")})
		}},
		{usage: "page.upload <page_id> <--ref|--css|--role+--name|--label> <file>", aliases: []string{"upload"}, positionals: []string{"page_id", "file"}, newArgs: func() any { return &UploadArgs{} }, run: func(ctx context.Context, c wire.Caller, a any) (any, error) {
			return s.Upload(ctx, c, *a.(*UploadArgs))
		}},
		{usage: "download.list <page_id>", aliases: []string{"downloads"}, positionals: []string{"page_id"}, newArgs: func() any { return &PageArgs{} }, run: func(ctx context.Context, c wire.Caller, a any) (any, error) {
			return s.DownloadList(ctx, c, *a.(*PageArgs))
		}},
		{usage: "download.get <download_id>", positionals: []string{"download_id"}, newArgs: func() any { return &DownloadArgs{} }, run: func(ctx context.Context, c wire.Caller, a any) (any, error) {
			return s.DownloadGet(ctx, c, *a.(*DownloadArgs))
		}},
		{usage: "download.wait <download_id>", positionals: []string{"download_id"}, newArgs: func() any { return &DownloadArgs{} }, run: func(ctx context.Context, c wire.Caller, a any) (any, error) {
			return s.DownloadWait(ctx, c, *a.(*DownloadArgs))
		}},
		{usage: "download.cancel <download_id>", positionals: []string{"download_id"}, newArgs: func() any { return &DownloadArgs{} }, run: func(ctx context.Context, c wire.Caller, a any) (any, error) {
			return s.DownloadCancel(ctx, c, *a.(*DownloadArgs))
		}},
		{usage: "download.export <download_id> <path>", positionals: []string{"download_id", "path"}, newArgs: func() any { return &DownloadArgs{} }, run: func(ctx context.Context, c wire.Caller, a any) (any, error) {
			return s.DownloadExport(ctx, c, *a.(*DownloadArgs))
		}},
		{usage: "download.read <download_id> [--offset N] [--limit N]", positionals: []string{"download_id"}, newArgs: func() any { return &DownloadReadArgs{} }, run: func(ctx context.Context, c wire.Caller, a any) (any, error) {
			return s.DownloadRead(ctx, c, *a.(*DownloadReadArgs))
		}},
	}
	// 页面动作（click/fill/type/press/hover/scroll/drag/set）同形：
	// locator flags（--ref/--css/--role+--name/--label）+ --text/--key/--value/--x/--y/--after。
	for _, name := range []string{"click", "fill", "type", "press", "hover", "scroll", "drag", "set"} {
		action := name
		subs = append(subs, browserSub{
			usage:       "page." + action + " <page_id> <--ref|--css|--role+--name|--label> [--text T|--key K|--value V|--x N --y N] [--after none|summary|observation|image]",
			positionals: []string{"page_id"},
			newArgs:     func() any { return &ActionArgs{} },
			run: func(ctx context.Context, c wire.Caller, a any) (any, error) {
				return s.action(action)(ctx, c, *a.(*ActionArgs))
			},
		})
	}
	// 历史 CLI 别名（back/forward/reload）。
	for name, delta := range map[string]int{"back": -1, "forward": 1, "reload": 0} {
		d := delta
		subs = append(subs, browserSub{
			usage:       "page." + name + " <page_id>",
			aliases:     []string{name},
			positionals: []string{"page_id"},
			newArgs:     func() any { return &PageArgs{} },
			run: func(ctx context.Context, c wire.Caller, a any) (any, error) {
				return s.history(ctx, c, *a.(*PageArgs), d)
			},
		})
	}
	return subs
}

const browserHelp = `usage: browser <subcommand> [args] [--json]

子命令（用 browser <sub> --help 语义自查参数；viewer 契约见 --json）：
  status                      浏览器服务状态
  page.list                   列出页面（别名 pages）
  page.create [url]           新建页面（别名 open；--width/--height）
  page.navigate <page_id> <url>（别名 navigate）
  page.close <page_id>        （别名 close）
  page.observe <page_id>      观察页面（别名 observe；--query/--limit/--image）
  page.wait <page_id>         等待条件（--text T | --url U | --load | --ref/--css/--role+--name/--label + --state S；--timeout_ms N）
  page.events <page_id>       页面事件（--cursor/--kind）
  page.dialog.resolve <page_id> <dialog_id> [--accept|--accept=false] [--text T]
  page.evaluate <page_id> <code...>（别名 eval）
  page.upload <page_id> <locator-flags> <file>
  page.<click|fill|type|press|hover|scroll|drag|set> <page_id> <locator-flags> [选项]
  page.<back|forward|reload> <page_id>
  download.list <page_id>（别名 downloads）
  download.<get|wait|cancel> <download_id>
  download.export <download_id> <path>
  download.read <download_id> [--offset N] [--limit N]

locator flags：--ref R | --css C | --role R --name N | --label L（四选一）。
输出：stdout 只放约定 JSON（--json 紧凑）；诊断与警告写 stderr。
stream（page.frames/page.input）是 RTC 私有端点，不在本指令面。`

// VshCommand 构造 browser 的 vsh 指令。
// callerOf 由执行上下文构造业务身份（host 装配注入 vshglue.CallerFromContext
// ——browser 不反向依赖 vsh glue）。
func (s *Service) VshCommand(callerOf func(context.Context) wire.Caller) commands.Command {
	subs := s.subcommands()
	byName := map[string]browserSub{}
	for _, sub := range subs {
		name := strings.Fields(sub.usage)[0]
		byName[name] = sub
		for _, a := range sub.aliases {
			byName[a] = sub
		}
	}
	return commands.DefineCommand("browser", func(ctx context.Context, inv *commands.Invocation) error {
		// --json 任意位置生效（viewer 契约：stdout 只放约定 JSON）。
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
			fmt.Fprintln(inv.Stdout, browserHelp)
			return nil
		}
		sub, ok := byName[args[0]]
		if !ok {
			return commands.Exitf(inv, 2, "browser: unknown subcommand %q\n%s", args[0], browserHelp)
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
			return commands.Exitf(inv, 1, "browser %s: %s", args[0], err)
		}
		// 警告写 stderr（不污染 stdout 的约定 JSON）。
		if r, ok := v.(Result); ok && len(r.Warnings) > 0 {
			for _, w := range r.Warnings {
				fmt.Fprintf(inv.Stderr, "warning: %s\n", w)
			}
		}
		var raw []byte
		if jsonOut {
			raw, err = json.Marshal(v)
		} else {
			raw, err = json.MarshalIndent(v, "", "  ")
		}
		if err != nil {
			return commands.Exitf(inv, 1, "browser %s: encode result: %s", args[0], err)
		}
		fmt.Fprintln(inv.Stdout, string(raw))
		return nil
	})
}
