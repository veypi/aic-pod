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
	"browser": {
		Desc: "control a web browser (native; only on browser-extension/desktop hosts)",
		Help: `browser <subcommand> [args...] — browser automation（平台自有指令集）

实现端：Chrome 插件 / desktop 壳原生 JS（CDP）；cloud 与普通 host 不提供。

Start here:
  browser snapshot             Accessibility tree with @refs (for AI)
  browser snapshot -i          Interactive elements only
  Every element gets a @ref; other actions target it with @<ref>:
  browser click @e2            Click by ref from snapshot

Supported (13):
  open <url>                   Navigate to URL in the AI workspace tab (http/https only)
  read [url]                   Extract readable page text (no url = current page)
  click <sel|@ref>            Click element (CSS selector or @ref from snapshot)
  eval <js>                   Run JS via CDP (DevTools semantics; use for type/fill/navigation workarounds)
  get <what> [sel]            text / html / title / url / value / attr <sel> <name> / count / box / styles
  network [id|requests] [--filter s] [--type t] [--method m] [--status n] [--limit n] [--clear]
                               List / detail network requests (id or "requests" = list)
  screenshot [--quality N] [--full]  Save JPEG (--full: full page); also returns image_data
  snapshot [-i] [-c] [-d N] [-s sel]  Accessibility tree with @refs (stale refs require re-snapshot)
  tab <new|list|close|N>       Manage tabs inside the AI workspace (never activates/steals focus)
  wait <sel|ms> [--url g] [--load l] [--fn js] [--text t] [--download f]
                               Wait for selector/ms/condition (default 30s)
  download <sel> <path>       Click element to trigger a download (waits 30s)
  close                        Close the AI workspace tab (re-created on next command)
  sleep <dur>                 Sleep (e.g. 1s, 500ms)

输入/导航/滚动等未列能力用 eval <js> 替代（如 el.value=...; el.dispatchEvent(new Event('input'))）

Behavior:
  stateful: serialized per (session, host) — click/snapshot races corrupt @refs
  页面一旦发生任何变化必须重新 snapshot 再进行下一次 ref 交互
  工作区模式所有操作在 AI 专属标签页/窗口，不激活不抢焦点`,
	},
	"cua": {
		Desc: "drive native desktop GUI apps via cua-driver (desktop host only)",
		Help: `cua <subcommand> [flags...] — 本机 GUI 自动化（cua-driver MCP 桥接）

实现端：desktop 壳（Electron 主进程持有 cua-driver mcp 持久子进程）；
要求本机已安装 cua-driver 且授权（辅助功能/录屏归宿主 app）。

感知先行（snapshot-before-action 不可省）：
  cua apps                       列出应用（运行中/已安装，含 pid）
  cua windows [--pid N]          列出窗口（window_id/title/bounds）
  cua snapshot --pid N --window W [--png] [--grep 关键词 [--context N]]
                                 AX 可访问性树；正文全量落会话工作区
                                 .cua/（snap-*.txt），返回语义摘要（结构/界面
                                 文本/带 token 可操作控件/菜单折叠，深度细节
                                 进 txt）
                                 --png 另出窗口截图（snap-*.png 落盘）并以
                                 image_data 附返回（直接投喂模型视觉）；默认
                                 不带（省 token，需要看图时显式请求）
                                 --grep 过滤树：命中行+祖先链+前后行+token 图例
                                 （window_id 从 cua windows / launch 应答取）

动作（目标二选一：snapshot 给的 --token，或像素 --x/--y；--pid/--window 限定窗口）：
  cua click|dclick|rclick [--token T | --x X --y Y] [--pid N --window W]
  cua type --text "..."          插入文本（AX 元素或当前焦点）；含非 ASCII
                                 （中文等）自动改走剪贴板粘贴——逐键合成会被
                                 输入法吞或转候选
  cua key <key>                  单键（enter/tab/esc...）
  cua hotkey <combo>             组合键（cmd+c / ctrl+shift+s）
  cua scroll --direction up|down|left|right [--amount N]
  cua drag --x1 X --y1 Y --x2 X --y2 Y
  cua move --x X --y Y           移动光标
  cua front --pid N [--window W]  前台激活应用（窃取前台焦点，Danger 逐次审批；
                                 前台投递/IME 敏感输入的前提）
  cua set-value --token T --value V   设置非文本控件值（下拉/勾选/滑块）
  cua menu --pid N --path "File>Save" 调用原生菜单
  cua set-frame --pid N --window W --x X --y Y --width W --height H
  cua launch --app <name> [--url U]... 启动/唤起应用（可带 URL 开页）
  cua clipboard read|write [text]     系统剪贴板
  cua doctor                     环境自检（一次性 CLI，不走 MCP）

浏览器正解（typed browser 家族，页面操作优先于地址栏/像素）：
  cua bprepare --isolated          准备驱动自持隔离浏览器（不需登录态优先）
  cua bprepare --pid N --window W  绑定用户真实浏览器（开远程调试，Danger 逐次审批）
  cua browser-state [--pid N --window W | --target T --tab B] [--query q]
                                 绑定/快照（semantic 树）并存 target/tab
  cua navigate --url U           导航绑定 tab（先 bprepare/browser-state）
  cua bclick --ref R | --x X --y Y [--route trusted|dom_event]
  cua btype --ref R --text "..." [--mode insert_text|keystrokes] [--replace]
  cua bend                       结束浏览器会话（驱动回收临时 tab/设置）

投递语义：动作默认走驱动后台精确路由（AX 语义 → browser/CDP → 窗口本地
  指针 → PID 键盘 → 结构化拒绝），不动前台/真实指针；驱动 refused 时才考虑
  显式前台升级（平台不自动升）。
提级 Danger(3)（逐次审批）：
  --delivery foreground          前台投递（可能改变焦点/光标，用户可见接管；
                                 IME 敏感输入需先 cua front 激活）

输入法护栏（默认开启，自动；三平台）：键盘类动作（key/hotkey/type）执行前
  检测系统输入法，若是中文/日文 IME 则自动切到英文键盘布局——IME 会把
  Shift+A 这类组合键当输入法切换吃掉（2026-09-09 Blender 实测：搜狗拼音下
  Shift+A 退化成裸 a）。已是英文时零开销跳过；发生切换时响应末尾附 [ime]
  一行。非 ASCII 文本输入（type 含中文）平台无关地改走剪贴板粘贴。
  关闭：环境变量 AIC_CUA_IME_GUARD=0。

脚本批处理（多步自动化首选，免逐指令往返）：
  cua run --file <host绝对路径>  执行脚本文件（先 fs write 写好，再 run）
  cua run --code '<js>'          内联短脚本
  恒 Danger(3) 逐次审批（脚本全文随审批可见）；总时长≤300s，步数上限 500。
  脚本为 JS（支持 await/return，无 import），注入全局 cua 对象：
    目标   await cua.launch("Blender")；await cua.apps()；await cua.windows(pid)
           await cua.target({pid, window?})  绑定后动作免填 pid/window
    感知   const s = await cua.snapshot({png?})
           → {title,bounds,elements[],text_path,png_path?}
           s.find("标签") / s.findAll(/正则/) 模糊找元素（label/value 含即中）
    动作   cua.click(el|[x,y]|x,y) 元素优先 token、兑底 frame 中心；
           dclick/rclick 同理；cua.type("文本")（含中文自动走剪贴板）；
           cua.paste("长文本")（剪贴板写入+粘贴热键，长文本比 type 可靠）；
           cua.key("enter")；
           cua.hotkey("cmd+s")；cua.scroll("down",3)；cua.drag(x1,y1,x2,y2)；
           cua.move(x,y)；cua.setValue(token,v)；cua.menu("File>Save")；
           cua.setFrame(x,y,w,h)；cua.clipboardRead()/clipboardWrite(t)
    浏览器 await cua.bprepare({isolated:true})；cua.navigate(url)；
           cua.bclick({ref}|[x,y])；cua.btype(ref, text)；cua.browserState()；cua.bend()
    控制   await cua.sleep(ms)；cua.log(msg)；console.log 同 log
           await cua.front(pid) 前台激活目标应用（Danger；前台投递的前提）
    投递   动作默认后台精确路由，不动前台/真实指针；需要前台接管时带末参
           opts：{delivery:"foreground"}（真实全局事件——IME 活跃的文本框里
           后台合成修饰键会被输入法吃掉，换 foreground 穿透；需先 front 前台
           激活，否则事件落到别的 app）。scope 已移除。
  动作错误抛 JS 异常——可 try/catch 自适应重试；未捕获即终止。
  return 值与逐步 transcript（含各步耗时/错误）随应答返回，全文落 .cua/run-*.jsonl。
  脚本运行在 host 上（node），可直接 require('node:fs') 读写本机文件、读 .cua/
  下的 snapshot 正文；坐标随界面变化过期，界面变化后重新 snapshot 再交互。

注意：坐标会随界面变化过期，每次界面变化后必须重新 snapshot 再交互。`,
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
	} else if name == "browser" {
		level = proto.LevelWrite // 基础 = write；读类子命令动态降级在 browserRequired
	} else if name == "cua" {
		level = proto.LevelWrite // 基础 = write；读类子命令动态降级、scope/foreground 提级在 cuaRequired
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
