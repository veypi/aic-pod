package vcore

import (
	"sort"

	"github.com/veypi/aic-pod/libs/proto"
)

// CommandMeta 是指令元数据（§6.3 注册信息三件套）：
// Desc 为简短描述（commands 输出给 AI 做能力发现）；
// Help 为完整帮助文档（procs 拦截 `--help` 时内部返回，不下发到执行端）。
type CommandMeta struct {
	Desc string
	Help string
}

// commandMeta 是 exec 全部虚拟指令的元数据表（curl + git/browser/json/bg_*/commands）。
// 文件类指令（ls/rg/cp/mv/rm 等）已迁入 fs 指令集（8 action，静态 schema）。
// desc/help 与 levels.go 分级表同包维护，禁止各端另行声明。
var commandMeta = map[string]CommandMeta{
	"curl": {
		Desc: "download a URL to a file, or fetch content into output",
		Help: "curl [-L] [-o <path>] <url> [--max-size <MB>]\n" +
			"  HTTP(S) GET\n" +
			"  with -o: stream to <path> (requires fs write; target must not exist)\n" +
			"  without -o: output into content (task-managed: logged, auto-background on timeout,\n" +
			"    first-1000-lines truncation; bg_wait to continue)\n" +
			"    binary content is rejected on sniff — use -o <path> to save it as a file\n" +
			"  --max-size default 1024MB, cap 10240MB (aborts over-limit downloads)\n" +
			"  cloud: loopback/private/link-local targets rejected (SSRF guard)",
	},
	"git": {
		Desc: "version control",
		Help: "git [-C <path>] <subcommand> [args...]\n" +
			"  version control, semantics aligned with the git CLI\n" +
			"  subcommands: status/log/diff/branch (read) | init/clone/add/commit/pull (write)\n" +
			"               | push/checkout (danger — may discard local changes or send remote)\n" +
			"  clone is shallow by default (--depth 1, single-branch); --depth <n> overrides, --full clones all history\n" +
			"  remote auth: host uses local git credentials; cloud is anonymous-only",
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
	"grant": {
		Desc: "request an allow-list grant (fs/net/ssh; approval required)",
		Help: "grant <domain> <target> [--temp|--permanent]\n" +
			"  add a target to a domain's allow list (always requires approval, level 4)\n" +
			"  domains:\n" +
			"    fs  <path>        writable root (fs writes and sandbox write access)\n" +
			"    net <host:port>   outbound network target for sandboxed processes\n" +
			"    ssh <host[:port]> ssh tool target (bare host = all ports)\n" +
			"  --temp (default): this session only, lost on host restart\n" +
			"  --permanent: persisted to the domain's allow list in host config\n" +
			"  targets in the domain's deny list cannot be granted (a narrower allow entry in host config can override)",
	},
	"ssh": {
		Desc: "run a command on a remote host via SSH (allow-listed targets only)",
		Help: "ssh <target> [remote command...]\n" +
			"  target = [user@]host[:port] or an alias from ~/.ssh/config\n" +
			"  the target must be in the ssh allow list (ssh_policy/ssh_allow);\n" +
			"  request access via: grant ssh <host[:port]> [--temp|--permanent]\n" +
			"  flags before the target are not accepted (the tool owns them);\n" +
			"  everything after the target is passed to the remote side verbatim\n" +
			"  auth: the device's existing keys / ssh-agent / ~/.ssh/config\n" +
			"  (no password prompts — non-interactive); host keys: accept-new",
	},
	"scp": {
		Desc: "copy files between this host and a remote host over SSH (allow-listed targets only)",
		Help: "scp [-r] [-p] [-q] [-P port] <source> <target>\n" +
			"  exactly one side is remote: [user@]host:path (aliases from ~/.ssh/config work)\n" +
			"  the remote target must be in the ssh allow list (ssh_policy/ssh_allow);\n" +
			"  request access via: grant ssh <host[:port]> [--temp|--permanent]\n" +
			"  flags are whitelisted (-r/-p/-q/-P); the tool owns everything else\n" +
			"  local paths are checked against the fs policy (fs_deny = no read/write)\n" +
			"  remote-to-remote copy is not supported; auth: device keys / agent\n" +
			"  (batch mode, no password prompts); host keys: accept-new",
	},
	"json": {
		Desc: "view and edit JSON files",
		Help: "json <view|set|del|append|merge> ... — JSON file tool (no external deps)\n" +
			"\n" +
			"Subcommands:\n" +
			"  view    structure skeleton or extract a subtree (large-file safe)\n" +
			"  set     set a value at a dotted path\n" +
			"  del     delete a dotted path\n" +
			"  append  append a value to an array at a dotted path\n" +
			"  merge   shallow-merge an object (Object.assign({}, doc, new))\n" +
			"\n" +
			"Use `json <subcommand> --help` for details.",
	},
}

// jsonSubHelp 是 json 子命令级帮助文档（`json <sub> --help` 内部返回）。
var jsonSubHelp = map[string]string{
	"view": "json view <path> [--key <dotted.path>] [--depth N] [--values] [--raw] [--compact]\n" +
		"  view a JSON file — default: structure skeleton (no values, large-file safe)\n" +
		"  <path>     JSON file (must be valid JSON, max 64MB)\n" +
		"  --key      extract a subtree: dotted segments a.b.c, array index [0] or .0\n" +
		"             (missing key → error); output pretty JSON, --compact: single line\n" +
		"  --depth N  skeleton depth limit (default 4, max 10); deeper nodes show type only\n" +
		"  --values   include scalar values in skeleton (truncated 200 chars)\n" +
		"  --raw      raw file bytes (truncated; not combinable with --key)",
	"set": "json set <path> <key> <value>\n" +
		"  set a value at a dotted path (missing containers auto-created)\n" +
		"  <key>    dotted path: a.b.c / a.b[0].c / a.b.0.c\n" +
		"  <value>  parsed as JSON literal (true/false/null/number/object/array);\n" +
		"           otherwise kept as string\n" +
		"  output: {\"ok\":true,\"path\":\"...\",\"ops\":[\"set a.b\"],\"bytes\":N}",
	"del": "json del <path> <key>\n" +
		"  delete a dotted path (missing key → error)\n" +
		"  output: {\"ok\":true,\"path\":\"...\",\"ops\":[\"del a.b\"],\"bytes\":N}",
	"append": "json append <path> <key> <value>\n" +
		"  append a value to the array at <key>\n" +
		"  <key> not existing → created as [value]; target not an array → error\n" +
		"  output: {\"ok\":true,\"path\":\"...\",\"ops\":[\"append a.b\"],\"bytes\":N}",
	"merge": "json merge <path> <json>\n" +
		"  shallow-merge: doc = Object.assign({}, doc, parse(<json>))\n" +
		"  top-level keys added/overwritten; nested objects and arrays replaced wholesale;\n" +
		"  document must be a JSON object\n" +
		"  output: {\"ok\":true,\"path\":\"...\",\"ops\":[\"merge\"],\"bytes\":N}",
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
	} else if name == "json" {
		level = proto.LevelRead // 基础 = read（view）；set/del/append/merge 动态提升在 jsonRequired
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
