package vcore

import (
	"sort"

	"github.com/veypi/aic-pod/sdk/proto"
)

// CommandMeta 是指令元数据（§6.3 注册信息三件套）：
// Desc 为简短描述（commands 输出给 AI 做能力发现）；
// Help 为完整帮助文档（procs 拦截 `--help` 时内部返回，不下发到执行端）。
type CommandMeta struct {
	Desc string
	Help string
}

// commandMeta 是全部虚拟指令的元数据表（核心 8 + git/browser/bg_*/commands）。
// desc/help 与 levels.go 分级表同包维护，禁止各端另行声明。
var commandMeta = map[string]CommandMeta{
	"ls": {
		Desc: "list directory entries",
		Help: "ls [-l] [-a] [-t] [-h] [path]\n" +
			"  list directory entries, sorted by name (UTF-8 byte order)\n" +
			"  -l     include size and mtime (unix seconds) columns\n" +
			"  -a     include entries starting with '.' (hidden by default)\n" +
			"  -t     sort by mtime, newest first (ties by name)\n" +
			"  -h     human-readable size (1024-based, with -l; GNU ls -h aligned)\n" +
			"  path   defaults to workdir",
	},
	"rg": {
		Desc: "search content or list files",
		Help: "rg <pattern> <path> | rg --files [-g <glob>]... [<path>]\n" +
			"  content search: recursive; output {path}:{line}:{content}\n" +
			"  -i        case-insensitive\n" +
			"  -l        print only matching file paths\n" +
			"  -m N      per-file match limit\n" +
			"  -n        print line numbers (default behavior, accepted for compatibility)\n" +
			"  -c        print only match count per file\n" +
			"  -w        match whole words only\n" +
			"  -g <glob> filename glob (basename match, repeatable, OR semantics)\n" +
			"  --files   list files recursively instead of searching\n" +
			"  --hidden  include hidden files and directories (excluded by default, like real rg)\n" +
			"  regex: RE2/Rust regex semantics (no lookaround/backreference);\n" +
			"         use bash -c \"grep -P ...\" on a physical host for PCRE",
	},
	"tree": {
		Desc: "print directory tree (JSON)",
		Help: "tree [path] [-L N]\n" +
			"  structured recursive directory tree, JSON output (always structured; no --json flag)\n" +
			"  path   defaults to workdir\n" +
			"  -L N   max depth (native tree semantics; --depth N accepted as alias), default 3, max 5;\n" +
			"         node cap 2000 (truncated=true)\n" +
			"  hidden items (.xxx) excluded entirely (GNU tree default; -a not supported);\n" +
			"  node_modules/vendor/__pycache__/dist/build/.next etc. listed as leaves without recursion",
	},
	"curl": {
		Desc: "download a URL to a file",
		Help: "curl -o <path> <url> [--max-size <MB>]\n" +
			"  HTTP(S) GET, streamed to <path>; target must not exist\n" +
			"  --max-size default 1024MB, cap 10240MB (aborts and cleans partial file)\n" +
			"  cloud: loopback/private/link-local targets rejected (SSRF guard)",
	},
	"rm": {
		Desc: "remove files or directories",
		Help: "rm [-r] <path>\n" +
			"  remove a file or empty directory; -r for recursive delete\n" +
			"  root directories are hard-protected (cannot be removed)",
	},
	"mkdir": {
		Desc: "create directories",
		Help: "mkdir [-p] <path>\n" +
			"  without -p: parent must exist and target must not exist\n" +
			"  -p: recursive create, idempotent on existing",
	},
	"cp": {
		Desc: "copy files or directories",
		Help: "cp [-r] <src> <dst>\n" +
			"  copy within the same host (no cross-host copy)\n" +
			"  -r required for directories; dst must not exist;\n" +
			"  copying a directory into itself is rejected",
	},
	"mv": {
		Desc: "move or rename files or directories",
		Help: "mv <src> <dst>\n" +
			"  move within the same host; dst must not exist;\n" +
			"  root directories are hard-protected",
	},
	"git": {
		Desc: "version control",
		Help: "git [-C <path>] <subcommand> [args...]\n" +
			"  version control, semantics aligned with the git CLI\n" +
			"  subcommands: status/log/diff/branch (read) | init/clone/add/commit/pull (write)\n" +
			"               | push/checkout (danger — may discard local changes or send remote)\n" +
			"  remote auth: host uses local git credentials; cloud is anonymous-only",
	},
	"browser": {
		Desc: "control a web browser (agent-browser CLI)",
		Help: `browser <subcommand> [args...] — fast browser automation for AI agents

Start here:
  browser snapshot             Accessibility tree with @refs (for AI)
  browser snapshot -i          Interactive elements only
  Every element gets a @ref; other actions target it with @<ref>:
  browser click @e2            Click by ref from snapshot

Core Commands:
  open <url>                   Navigate to URL (http/https only)
  read [url]                   Fetch agent-readable text (NOTE: navigates the page)
  click <sel> / dblclick <sel> Click / double-click element (or @ref)
  type <sel> <text>            Type into element
  fill <sel> <text>            Clear and fill
  press <key>                  Press key (Enter, Tab, Control+a)
  keyboard type <text>         Type with real keystrokes (no selector)
  hover <sel> / focus <sel>    Hover / focus element
  check <sel> / uncheck <sel>  Check / uncheck checkbox
  select <sel> <val...>        Select dropdown option
  drag <src> <dst>             Drag and drop
  upload <sel> <files...>      Upload files (cloud: sources must be inside $SESSION/$USER/$AGENT)
  download <sel> <path>        Download file by clicking element (cloud: path inside $SESSION)
  scroll <dir> [px]            Scroll (up/down/left/right)
  scrollintoview <sel>         Scroll element into view
  wait <sel|ms>                Wait for element or time
  screenshot [--full]          Take screenshot; auto-saved as timestamped jpg (path arg ignored; image is NOT returned — use fs read on the saved path to view it)
  pdf <path>                   Save page as PDF
  snapshot [-i] [-c] [-d N] [-s sel]  Accessibility tree with refs
  eval <js>                    Run JavaScript (awaits promises)
  close [--all]                Close browser (--all closes every session)

Navigation:  back / forward / reload

Get Info:  browser get <what> [selector]
  text, html, value, title, url, count, box, styles (selector required except title/url/value)
  attr:  browser get attr <selector> <name>

Check State:  browser is <what> <selector>   visible / enabled / checked

Find Elements:  browser find <locator> <value> <action> [text]
  role, text, label, placeholder, alt, title, testid, first, last, nth

Mouse:  browser mouse <action> [args]   move <x> <y>, down [btn], up [btn], wheel <dy> [dx]

Browser Settings:  browser set <setting> [value]
  viewport <w> <h>, device <name>, geo <lat> <lng>, offline [on|off], media [dark|light]

Network:  browser network <sub> [args]
  route <url> [--abort|--body <json>]  Intercept/mock requests
  unroute [url]                        Remove route
  requests [--clear] [--filter p] [--type t] [--method m] [--status c]   List requests
  request <id>                         Request detail (with body)
  har <start|stop> [path]              Record/export HAR

Storage:
  cookies [get|set|clear]      Manage cookies
  storage <local|session>      Manage web storage

Tabs:  browser tab [new|list|close|<n>]   Manage tabs (monotonic ids: t1, t2, ...)

Diff:  browser diff snapshot | diff screenshot --baseline | diff url <u1> <u2>

Debug:
  trace start | trace stop [path]       Chrome DevTools trace
  profiler start|stop [path]            Chrome DevTools profile
  record start <path> [url] | record stop  Video recording (WebM)
  console [--clear] / errors [--clear]  Console logs / page errors
  highlight <sel> / inspect / clipboard <op> [text]

Batch:  browser batch [--bail] ["cmd" ...]   Execute multiple commands sequentially

Auth Vault:  browser auth save <name> | auth login <name> | auth list | auth show <name> | auth delete <name>

Sessions:  browser session | session list

Others:  browser connect <port|url> | pushstate <url> | mcp | skills get core

Behavior:
  stateful: serialized per (session, host) — click/snapshot races corrupt @refs
  download/wait can background: returns background=true + id, then bg_wait/bg_kill
  close auto-restarts: next command launches a fresh browser (cookies/storage reset)`,
	},
	"bg_list": {
		Desc: "list background processes of this session",
		Help: "bg_list\n" +
			"  list running background processes (id, command, elapsed)",
	},
	"bg_wait": {
		Desc: "wait for a background process",
		Help: "bg_wait <id> [--wait N]\n" +
			"  wait up to N seconds (default 30) for background process result;\n" +
			"  returns background=true if still running",
	},
	"bg_kill": {
		Desc: "terminate a background process",
		Help: "bg_kill <id>\n" +
			"  terminate a background process of this session",
	},
	"commands": {
		Desc: "discover available commands on a target",
		Help: "commands\n" +
			"  capability discovery: list declared commands (name + desc) of the target;\n" +
			"  use `action --help` for the full help of any command",
	},
}

// Meta 返回指令元数据（desc/help）。未知指令返回 ok=false。
func Meta(name string) (CommandMeta, bool) {
	m, ok := commandMeta[name]
	return m, ok
}

// Decl 组装指令的完整注册声明 {name, desc, help, level}（§6.3）：
// level 取基础分级（levels.go 同源，不带 argv 的动态提升——提升由判断端按需调用
// ExecRequired/ExecRequiredIn）。未知指令返回 ok=false。
func Decl(name string) (proto.CommandDecl, bool) {
	m, ok := commandMeta[name]
	if !ok {
		return proto.CommandDecl{}, false
	}
	level := proto.LevelDanger // 未知指令 Danger 兜底（与 ExecRequired 一致）
	if lv, ok := execCoreLevels[name]; ok {
		level = lv
	} else if name == "git" {
		level = proto.LevelRead // 基础 = read（读操作）；push/reset/checkout 动态提升在 gitRequired
	} else if name == "browser" {
		level = proto.LevelWrite // 基础 = write；eval/upload 动态提升在 browserRequired
	}
	return proto.CommandDecl{Name: name, Desc: m.Desc, Help: m.Help, RequiredLevel: level}, true
}

// AllDecls 返回全部已知虚拟指令的注册声明（排序，供 caps 上报与 commands 聚合）。
func AllDecls() []proto.CommandDecl {
	names := make([]string, 0, len(commandMeta))
	for n := range commandMeta {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]proto.CommandDecl, 0, len(names))
	for _, n := range names {
		if d, ok := Decl(n); ok {
			out = append(out, d)
		}
	}
	return out
}
