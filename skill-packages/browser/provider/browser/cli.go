package browser

// cli.go 是 browser 包 CLI 的命令面（v6 P5，原内建 vsh 指令面随拆包改造为
// 包内 CLI）：process provider（cli/bin/browser）经 skillproc 把
// argv/cwd 全量转发给 svc provider，svc 调 Run 执行。输出契约不变：stdout 只放
// 约定 JSON（--json 紧凑单行，默认缩进），提示/诊断/警告写 stderr；退出码非零
// 不能当成功数据使用。
//
// page.frames / page.input 不在此——它们是 stream 端点（manifest streams[]，
// 端点名 = 流名全名 page.frames/page.input）。

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

// cliSub 是一个子命令的声明。
type cliSub struct {
	usage       string
	aliases     []string
	positionals []string
	newArgs     func() any
	run         func(ctx context.Context, a any) (any, error)
	// special 接管原始参数（evaluate 等整段文本参数）。
	special func(ctx context.Context, args []string) (any, error)
}

func (s *Service) subcommands() []cliSub {
	subs := []cliSub{
		{usage: "status", newArgs: func() any { return &Empty{} }, run: func(ctx context.Context, a any) (any, error) {
			return s.Status(ctx, Empty{})
		}},
		{usage: "page.list", aliases: []string{"pages"}, newArgs: func() any { return &Empty{} }, run: func(ctx context.Context, a any) (any, error) {
			return s.List(ctx, Empty{})
		}},
		{usage: "page.create [url] [--width N] [--height N]", aliases: []string{"open"}, positionals: []string{"url?"}, newArgs: func() any { return &CreateArgs{} }, run: func(ctx context.Context, a any) (any, error) {
			return s.Create(ctx, *a.(*CreateArgs))
		}},
		{usage: "page.navigate <page_id> <url>", aliases: []string{"navigate"}, positionals: []string{"page_id", "url"}, newArgs: func() any { return &NavigateArgs{} }, run: func(ctx context.Context, a any) (any, error) {
			return s.Navigate(ctx, *a.(*NavigateArgs))
		}},
		{usage: "page.close <page_id>", aliases: []string{"close"}, positionals: []string{"page_id"}, newArgs: func() any { return &PageArgs{} }, run: func(ctx context.Context, a any) (any, error) {
			return s.ClosePage(ctx, *a.(*PageArgs))
		}},
		{usage: "page.observe <page_id> [--query Q] [--limit N] [--image]", aliases: []string{"observe"}, positionals: []string{"page_id"}, newArgs: func() any { return &ObserveArgs{} }, run: func(ctx context.Context, a any) (any, error) {
			return s.Observe(ctx, *a.(*ObserveArgs))
		}},
		{usage: "page.wait <page_id> [--text T|--url U|--load|--ref/--css/--role+--name/--label + --state S] [--timeout_ms N]", aliases: []string{"wait"}, special: func(ctx context.Context, args []string) (any, error) {
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
			return s.Wait(ctx, a)
		}},
		{usage: "page.events <page_id> [--cursor N] [--kind K]", aliases: []string{"events"}, positionals: []string{"page_id"}, newArgs: func() any { return &EventsArgs{} }, run: func(ctx context.Context, a any) (any, error) {
			return s.Events(ctx, *a.(*EventsArgs))
		}},
		{usage: "page.dialog.resolve <page_id> <dialog_id> [--accept|--accept=false] [--text T]", aliases: []string{"dialog"}, positionals: []string{"page_id", "dialog_id"}, newArgs: func() any { return &DialogArgs{} }, run: func(ctx context.Context, a any) (any, error) {
			return s.Dialog(ctx, *a.(*DialogArgs))
		}},
		{usage: "page.evaluate <page_id> <code...>", aliases: []string{"eval"}, special: func(ctx context.Context, args []string) (any, error) {
			if len(args) < 2 {
				return nil, wire.Fail("invalid_argument", "usage: page.evaluate <page_id> <code...>")
			}
			return s.Evaluate(ctx, EvaluateArgs{PageID: args[0], Code: strings.Join(args[1:], " ")})
		}},
		{usage: "page.upload <page_id> <--ref|--css|--role+--name|--label> <file>", aliases: []string{"upload"}, positionals: []string{"page_id", "file"}, newArgs: func() any { return &UploadArgs{} }, run: func(ctx context.Context, a any) (any, error) {
			return s.Upload(ctx, *a.(*UploadArgs))
		}},
		{usage: "download.list <page_id>", aliases: []string{"downloads"}, positionals: []string{"page_id"}, newArgs: func() any { return &PageArgs{} }, run: func(ctx context.Context, a any) (any, error) {
			return s.DownloadList(ctx, *a.(*PageArgs))
		}},
		{usage: "download.get <download_id>", positionals: []string{"download_id"}, newArgs: func() any { return &DownloadArgs{} }, run: func(ctx context.Context, a any) (any, error) {
			return s.DownloadGet(ctx, *a.(*DownloadArgs))
		}},
		{usage: "download.wait <download_id>", positionals: []string{"download_id"}, newArgs: func() any { return &DownloadArgs{} }, run: func(ctx context.Context, a any) (any, error) {
			return s.DownloadWait(ctx, *a.(*DownloadArgs))
		}},
		{usage: "download.cancel <download_id>", positionals: []string{"download_id"}, newArgs: func() any { return &DownloadArgs{} }, run: func(ctx context.Context, a any) (any, error) {
			return s.DownloadCancel(ctx, *a.(*DownloadArgs))
		}},
		{usage: "download.export <download_id> <path>", positionals: []string{"download_id", "path"}, newArgs: func() any { return &DownloadArgs{} }, run: func(ctx context.Context, a any) (any, error) {
			return s.DownloadExport(ctx, *a.(*DownloadArgs))
		}},
		{usage: "download.read <download_id> [--offset N] [--limit N]", positionals: []string{"download_id"}, newArgs: func() any { return &DownloadReadArgs{} }, run: func(ctx context.Context, a any) (any, error) {
			return s.DownloadRead(ctx, *a.(*DownloadReadArgs))
		}},
	}
	// 页面动作（click/fill/type/press/hover/scroll/drag/set）同形：
	// locator flags（--ref/--css/--role+--name/--label）+ --text/--key/--value/--x/--y/--after。
	for _, name := range []string{"click", "fill", "type", "press", "hover", "scroll", "drag", "set"} {
		action := name
		subs = append(subs, cliSub{
			usage:       "page." + action + " <page_id> <--ref|--css|--role+--name|--label> [--text T|--key K|--value V|--x N --y N] [--after none|summary|observation|image]",
			positionals: []string{"page_id"},
			newArgs:     func() any { return &ActionArgs{} },
			run: func(ctx context.Context, a any) (any, error) {
				return s.action(action)(ctx, *a.(*ActionArgs))
			},
		})
	}
	// 历史 CLI 别名（back/forward/reload）。
	for name, delta := range map[string]int{"back": -1, "forward": 1, "reload": 0} {
		d := delta
		subs = append(subs, cliSub{
			usage:       "page." + name + " <page_id>",
			aliases:     []string{name},
			positionals: []string{"page_id"},
			newArgs:     func() any { return &PageArgs{} },
			run: func(ctx context.Context, a any) (any, error) {
				return s.history(ctx, *a.(*PageArgs), d)
			},
		})
	}
	return subs
}

const Help = `usage: browser <subcommand> [args] [--json]

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

// Run 执行一条 browser CLI 命令（argv 不含程序名），返回进程退出码。
// cwd 是调用方工作目录（skillproc invoke Cwd = CLI 进程 cwd）：page.upload 与
// download.export 的相对文件参数在此绝对化——svc 进程 cwd 是包目录，与调用方
// 无关。
func (s *Service) Run(ctx context.Context, argv []string, cwd string, stdout, stderr io.Writer) int {
	subs := s.subcommands()
	byName := map[string]cliSub{}
	for _, sub := range subs {
		name := strings.Fields(sub.usage)[0]
		byName[name] = sub
		for _, a := range sub.aliases {
			byName[a] = sub
		}
	}
	// --json 任意位置生效（viewer 契约：stdout 只放约定 JSON）。
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
		fmt.Fprintf(stderr, "browser: unknown subcommand %q\n%s\n", args[0], Help)
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
		fmt.Fprintf(stderr, "browser %s: %s\n", args[0], err)
		return 1
	}
	// 警告写 stderr（不污染 stdout 的约定 JSON）。
	if r, ok := v.(Result); ok && len(r.Warnings) > 0 {
		for _, w := range r.Warnings {
			fmt.Fprintf(stderr, "warning: %s\n", w)
		}
	}
	var raw []byte
	if jsonOut {
		raw, err = json.Marshal(v)
	} else {
		raw, err = json.MarshalIndent(v, "", "  ")
	}
	if err != nil {
		fmt.Fprintf(stderr, "browser %s: encode result: %s\n", args[0], err)
		return 1
	}
	fmt.Fprintln(stdout, string(raw))
	return 0
}

// absolutize 把文件参数按调用方 cwd 绝对化（svc 进程 cwd 是包目录）。
func absolutize(parsed any, cwd string) {
	join := func(p string) string {
		if p == "" || filepath.IsAbs(p) || cwd == "" {
			return p
		}
		return filepath.Join(cwd, p)
	}
	switch a := parsed.(type) {
	case *UploadArgs:
		a.File = join(a.File)
	case *DownloadArgs:
		a.Path = join(a.Path)
	}
}
