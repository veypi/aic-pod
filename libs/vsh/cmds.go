package vsh

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/veypi/vsh/commands"
)

// PlatformDeps 平台命令依赖（cmds.go）。零值可用：未注入的能力降级为可读
// 报错（命令存在、help 自答、执行提示该端不可用）。Tasks 由 NewEngine 自动
// 接线，调用方无需填。
type PlatformDeps struct {
	// Tasks 后台任务登记表（引擎注入）。
	Tasks *TaskTable
	// Discoverable 命令发现展示过滤（§2.2 展示过滤，不是执行白名单）：
	// host/cloud 只展示核心自定义指令；nil = 展示全部（page——没有本机
	// 命令环境可供假定）。未列出的指令照常可执行。
	Discoverable func(name string) bool
	// Grant 执行授权修改（sessionKey=调用会话；domain: fs/net/ssh/cmd；
	// target: 路径/host:port/命令名；permanent=true 落盘永久生效——仅 host
	// 支持，cloud 无 permanent 档应拒绝）。返回给用户的可读结果文案；
	// 拒绝/失败返回 error。调用前已检查可信 grant_approved（cmds.go 门）。
	Grant func(ctx context.Context, sessionKey, domain, target string, permanent bool) (string, error)
	// GrantStatus 返回各域授权姿态与规则表（grant status——只读，不要求
	// grant_approved）。nil = 降级报错。
	GrantStatus func(ctx context.Context, sessionKey string) (string, error)
	// ListHosts 返回预格式化的主机表文本（表格形态由平台定——cloud 给
	// Markdown 表；host 端通常 nil 降级）。sessionKey=调用会话。
	ListHosts func(ctx context.Context, sessionKey string) (string, error)
	// SendUser 给用户发消息（通知通道）。sessionKey=调用会话。
	SendUser func(ctx context.Context, sessionKey, message string) error
	// Skill 注册中心与设备包操作（v6；nil 子命令降级报错）。
	Skill SkillDeps
}

// SkillDeps skill 命令的端侧实现（docs/skill.md §4，v6）。
// cloud = 注册中心直查（search/load/download 转存用户空间）；
// host = 设备包管理（download 安装、list 安装记录，P2 接线）。
type SkillDeps struct {
	// Search 可见 skill 清单（本地在前；query 空 = 全部；limit≤0 = 默认 20）。
	Search func(ctx context.Context, sessionKey, query string, limit int) (string, error)
	// Load 按裸寻址 ref 读取 SKILL.md 正文 + 能力清单（公开条目使用即关联/
	// 计数语义由端侧实现保持）。
	Load func(ctx context.Context, sessionKey, ref string) (string, error)
	// Download 获取包（cloud = 转存用户空间；host = 安装到设备）。version 空
	// = 当前发布版。
	Download func(ctx context.Context, sessionKey, ref, version string) (string, error)
	// List 已安装包记录（host；--json 由端侧定输出形态）。
	List func(ctx context.Context, sessionKey string) (string, error)
	// SetDisabled 禁用/启用已装包（host；禁用 = 根命令保留但调用显式失败，
	// 状态落 .install.json 持久）。
	SetDisabled func(ctx context.Context, sessionKey, name string, disabled bool) error
	// Remove 卸载已装包（host；停 provider + 删目录 + 解注册）。
	Remove func(ctx context.Context, sessionKey, name string) error
}

// RegisterPlatformCommands 注册平台命令：commands / bg / grant / list_hosts /
// send_user / skill。全部自带 help 文本（--help/-h 或无参数子命令自答）。
func RegisterPlatformCommands(reg *commands.Registry, deps PlatformDeps) error {
	for _, cmd := range []commands.Command{
		commands.DefineCommand("commands", deps.cmdCommands),
		commands.DefineCommand("bg", deps.cmdBG),
		commands.DefineCommand("grant", deps.cmdGrant),
		commands.DefineCommand("list_hosts", deps.cmdListHosts),
		commands.DefineCommand("send_user", deps.cmdSendUser),
		commands.DefineCommand("skill", deps.cmdSkill),
	} {
		if err := reg.Register(cmd); err != nil {
			return err
		}
	}
	return nil
}

// helpRequested 判定 help 诉求（--help/-h/help 首参）。
func helpRequested(args []string) bool {
	if len(args) == 0 {
		return false
	}
	return args[0] == "--help" || args[0] == "-h" || args[0] == "help"
}

// cmdCommands 列出命令（发现入口：exec 工具描述引导 commands +
// <cmd> --help）。host/cloud 经 Discoverable 只展示核心自定义指令（常见
// 内建不占发现目录；未列出的指令照常可执行——这是展示过滤不是白名单）；
// page（Discoverable=nil）展示全部已注册指令。
func (d PlatformDeps) cmdCommands(ctx context.Context, inv *commands.Invocation) error {
	if helpRequested(inv.Args) {
		fmt.Fprintln(inv.Stdout, "usage: commands — 列出当前环境可用的命令（用 <cmd> --help 查用法）")
		return nil
	}
	if inv.GetRegisteredCommands == nil {
		return commands.Exitf(inv, 1, "commands: registry unavailable")
	}
	for _, name := range inv.GetRegisteredCommands() {
		if d.Discoverable != nil && !d.Discoverable(name) {
			continue
		}
		fmt.Fprintln(inv.Stdout, name)
	}
	return nil
}

const bgHelp = `usage:
  bg list [--json]        列出本会话的后台任务（状态/日志路径）
  bg wait <id> [秒] [--json]  有界等待任务并报告其状态（共享本次 exec 前台预算）
  bg kill <id>            终止任务（与 cancel 同一执行句柄）

后台任务唯一来源是 exec 前台等待超时；输出读取用 FS 或 cat/tail 读日志路径。`

// bgJSON 是 bg list/wait --json 的固定输出形状（§4.5 黄金样例）：
// id/state/script/output/error_output 恒在；已完成时有 exit_code。
// state ∈ running/done/timeout/killed/error。
type bgJSON struct {
	ID          string `json:"id"`
	State       string `json:"state"`
	Script      string `json:"script"`
	Output      string `json:"output"`
	ErrorOutput string `json:"error_output"`
	ExitCode    *int   `json:"exit_code,omitempty"`
	Err         string `json:"error,omitempty"`
}

func taskJSON(t Task) bgJSON {
	j := bgJSON{ID: t.ID, State: t.Status, Script: t.Command, Output: t.LogOut, ErrorOutput: t.LogErr, Err: t.Err}
	if t.Status != "running" {
		code := t.ExitCode
		j.ExitCode = &code
	}
	return j
}

func writeJSON(inv *commands.Invocation, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	fmt.Fprintln(inv.Stdout, string(raw))
	return nil
}

// bgIdentity 取本次执行的任务归属（user, session）。
func bgIdentity(ctx context.Context) (string, string) {
	return OwnerFromContext(ctx), SessionFromContext(ctx)
}

func (d PlatformDeps) cmdBG(ctx context.Context, inv *commands.Invocation) error {
	if len(inv.Args) == 0 || helpRequested(inv.Args) {
		fmt.Fprintln(inv.Stdout, bgHelp)
		return nil
	}
	if d.Tasks == nil {
		return commands.Exitf(inv, 1, "bg: task table unavailable on this endpoint")
	}
	owner, session := bgIdentity(ctx)
	// --json 任意位置生效。
	jsonOut := false
	args := make([]string, 0, len(inv.Args))
	for _, a := range inv.Args {
		if a == "--json" {
			jsonOut = true
			continue
		}
		args = append(args, a)
	}
	switch args[0] {
	case "list":
		tasks := d.Tasks.List(owner, session)
		if jsonOut {
			out := make([]bgJSON, 0, len(tasks))
			for _, t := range tasks {
				out = append(out, taskJSON(t))
			}
			return writeJSON(inv, out)
		}
		if len(tasks) == 0 {
			fmt.Fprintln(inv.Stdout, "(no background tasks)")
			return nil
		}
		for _, t := range tasks {
			fmt.Fprintf(inv.Stdout, "%s\t%s\texit=%d\t%s\t%s\n", t.ID, t.Status, t.ExitCode, t.LogOut, t.Command)
		}
		return nil
	case "wait":
		if len(args) < 2 {
			return commands.Exitf(inv, 2, "usage: bg wait <id> [秒] [--json]")
		}
		// 禁止等待自身（后台执行里的 wait 只查询——等待链不派生）。
		if h := HandleFromContext(ctx); h != nil && h.TaskID() != "" && h.TaskID() == args[1] {
			return commands.Exitf(inv, 2, "bg wait: cannot wait on self (%s)", args[1])
		}
		// 有界等待（§2.4）：实际等待 = min(指定秒数, 本次 exec 剩余前台预算
		// 减 1 秒)；预算不足或在后台执行时（WaitBudgetRemaining ok=false）
		// 显式秒数同样归零——只查询不阻塞（阻塞会把整单 exec 耗到转后台，
		// 等待链反而膨胀）。同一脚本多次 wait 共用剩余预算。
		wait := time.Duration(0)
		if len(args) >= 3 {
			sec, err := strconv.Atoi(args[2])
			if err != nil || sec < 0 {
				return commands.Exitf(inv, 2, "bg wait: invalid seconds %q", args[2])
			}
			if remain, ok := WaitBudgetRemaining(ctx); ok {
				wait = time.Duration(sec) * time.Second
				if wait > remain {
					wait = remain
				}
			}
		} else if remain, ok := WaitBudgetRemaining(ctx); ok {
			wait = remain
		}
		task, err := d.Tasks.Wait(ctx, args[1], wait, owner, session)
		if err != nil {
			return commands.Exitf(inv, 1, "%s", err)
		}
		if jsonOut {
			if err := writeJSON(inv, taskJSON(task)); err != nil {
				return err
			}
		} else {
			fmt.Fprintf(inv.Stdout, "%s\t%s\texit=%d\t%s\t%s\n", task.ID, task.Status, task.ExitCode, task.LogOut, task.Command)
			if task.Err != "" {
				fmt.Fprintf(inv.Stdout, "error: %s\n", task.Err)
			}
		}
		if task.Status == "running" {
			return nil // 预算到期但任务仍在跑：退出码 0、状态透出
		}
		if task.ExitCode != 0 {
			return &commands.ExitError{Code: task.ExitCode}
		}
		return nil
	case "kill":
		if len(args) < 2 {
			return commands.Exitf(inv, 2, "usage: bg kill <id>")
		}
		if err := d.Tasks.Kill(args[1], owner, session); err != nil {
			return commands.Exitf(inv, 1, "%s", err)
		}
		fmt.Fprintf(inv.Stdout, "%s killed\n", args[1])
		return nil
	default:
		return commands.Exitf(inv, 2, "bg: unknown subcommand %q\n%s", args[0], bgHelp)
	}
}

const grantHelp = `usage:
  grant status                          查看各域授权姿态与规则表（只读）
  grant fs <路径> [--permanent]        授权文件访问（默认会话级临时授权；--permanent 落盘永久生效）
  grant net <host:port> [--permanent]  授权网络目标访问
  grant ssh <host:port> [--permanent]  授权 SSH 目标访问（host）
  grant cmd <命令名> [--permanent]     授权原生命令（host；授予解释器 = 授予该进程一切能力）

授权修改需要服务端审批：含 grant 的脚本在发送前审批（grant_approved），
无批准事实时本命令报 permission_denied、规则不变。`

func (d PlatformDeps) cmdGrant(ctx context.Context, inv *commands.Invocation) error {
	if len(inv.Args) == 0 || helpRequested(inv.Args) {
		fmt.Fprintln(inv.Stdout, grantHelp)
		return nil
	}
	// status 只读查询（不要求 grant_approved）。
	if inv.Args[0] == "status" {
		if d.GrantStatus == nil {
			return commands.Exitf(inv, 1, "grant: 此端未接状态查询")
		}
		text, err := d.GrantStatus(ctx, SessionFromContext(ctx))
		if err != nil {
			return commands.Exitf(inv, 1, "grant status: %s", err)
		}
		fmt.Fprintln(inv.Stdout, text)
		return nil
	}
	// grant 修改入口（唯一安全边界，§3.1）：每次实际写规则前读取可信
	// 上下文——覆盖字面、变量拼接、命令替换、eval/source 与嵌套脚本
	//（都执行到这里）。无批准事实直接 permission_denied，规则不变，
	// 不暂停、不转 waiting、不重放脚本。
	if !GrantApprovedFromContext(ctx) {
		return commands.Exitf(inv, 126, "permission_denied: grant 修改授权需要审批——由服务端批准本次脚本（grant_approved）后重发；AI 请向用户申请批准该脚本")
	}
	// --permanent 任意位置生效（grant fs /x --permanent / grant --permanent fs /x）。
	permanent := false
	args := make([]string, 0, len(inv.Args))
	for _, a := range inv.Args {
		if a == "--permanent" {
			permanent = true
			continue
		}
		args = append(args, a)
	}
	if len(args) < 2 {
		return commands.Exitf(inv, 2, "usage: grant <fs|net|ssh|cmd> <target> [--permanent]")
	}
	domain, target := args[0], args[1]
	switch domain {
	case "fs", "net", "ssh", "cmd":
	default:
		return commands.Exitf(inv, 2, "grant: unknown domain %q（支持 fs/net/ssh/cmd）", domain)
	}
	if d.Grant == nil {
		return commands.Exitf(inv, 1, "grant: 此端未接授权通道")
	}
	msg, err := d.Grant(ctx, SessionFromContext(ctx), domain, target, permanent)
	if err != nil {
		return commands.Exitf(inv, 1, "grant %s %s: %s", domain, target, err)
	}
	if msg == "" {
		msg = fmt.Sprintf("grant %s %s: 已授权", domain, target)
	}
	fmt.Fprintln(inv.Stdout, msg)
	return nil
}

const listHostsHelp = `usage: list_hosts — 列出可执行 host exec 的主机（id/name/os/在线状态）`

func (d PlatformDeps) cmdListHosts(ctx context.Context, inv *commands.Invocation) error {
	if helpRequested(inv.Args) {
		fmt.Fprintln(inv.Stdout, listHostsHelp)
		return nil
	}
	if d.ListHosts == nil {
		return commands.Exitf(inv, 1, "list_hosts: 此端未接主机目录")
	}
	text, err := d.ListHosts(ctx, SessionFromContext(ctx))
	if err != nil {
		return commands.Exitf(inv, 1, "list_hosts: %s", err)
	}
	fmt.Fprintln(inv.Stdout, text)
	return nil
}

const sendUserHelp = `usage: send_user <消息...> — 给用户发一条通知消息`

const skillHelp = `usage: skill <search|load|download|list|disable|enable|remove> — skill 注册中心与设备包管理
  skill search [关键词...] [--limit N]   列出可见 skill（本地在前）
  skill load <ref>                       读取 SKILL.md 正文 + 能力清单
  skill download <ref> [--version v]     获取包（cloud = 转存用户空间；host = 安装到设备）
  skill list                             已安装包记录（host）
  skill disable <name>                   禁用已装包（host；根命令保留但调用显式失败）
  skill enable <name>                    启用已装包（host）
  skill remove <name>                    卸载已装包（host；删目录 + 解注册）`

// cmdSkill skill 注册中心与设备包管理（v6：skills 独立工具废除，动词归 vsh）。
func (d PlatformDeps) cmdSkill(ctx context.Context, inv *commands.Invocation) error {
	if helpRequested(inv.Args) {
		fmt.Fprintln(inv.Stdout, skillHelp)
		return nil
	}
	if len(inv.Args) == 0 {
		return commands.Exitf(inv, 2, "usage: skill <search|load|download|list|disable|enable|remove>（--help 查看详情）")
	}
	sub, rest := inv.Args[0], inv.Args[1:]
	sid := SessionFromContext(ctx)
	switch sub {
	case "search":
		if d.Skill.Search == nil {
			return commands.Exitf(inv, 1, "skill: 此端未接注册中心查询")
		}
		limit := 0
		var words []string
		for i := 0; i < len(rest); i++ {
			if rest[i] == "--limit" && i+1 < len(rest) {
				v, err := strconv.Atoi(rest[i+1])
				if err != nil {
					return commands.Exitf(inv, 2, "skill search: --limit 需为数字")
				}
				limit = v
				i++
				continue
			}
			words = append(words, rest[i])
		}
		text, err := d.Skill.Search(ctx, sid, strings.Join(words, " "), limit)
		if err != nil {
			return commands.Exitf(inv, 1, "skill search: %s", err)
		}
		if text != "" {
			fmt.Fprintln(inv.Stdout, text)
		}
		return nil
	case "load":
		if d.Skill.Load == nil {
			return commands.Exitf(inv, 1, "skill: 此端未接注册中心查询")
		}
		if len(rest) == 0 {
			return commands.Exitf(inv, 2, "usage: skill load <ref>")
		}
		text, err := d.Skill.Load(ctx, sid, strings.Join(rest, " "))
		if err != nil {
			return commands.Exitf(inv, 1, "skill load: %s", err)
		}
		fmt.Fprintln(inv.Stdout, text)
		return nil
	case "download":
		if d.Skill.Download == nil {
			return commands.Exitf(inv, 1, "skill download: 此端未接包获取通道")
		}
		version := ""
		var words []string
		for i := 0; i < len(rest); i++ {
			if rest[i] == "--version" && i+1 < len(rest) {
				version = rest[i+1]
				i++
				continue
			}
			words = append(words, rest[i])
		}
		if len(words) == 0 {
			return commands.Exitf(inv, 2, "usage: skill download <ref> [--version v]")
		}
		text, err := d.Skill.Download(ctx, sid, strings.Join(words, " "), version)
		if err != nil {
			// 实现层错误已自带 skill download: 前缀（skillrun/fetch 同一口径），不再重包。
			return commands.Exitf(inv, 1, "%s", err)
		}
		if text != "" {
			fmt.Fprintln(inv.Stdout, text)
		}
		return nil
	case "list":
		if d.Skill.List == nil {
			return commands.Exitf(inv, 1, "skill list: 此端无设备安装记录（host 端命令）")
		}
		text, err := d.Skill.List(ctx, sid)
		if err != nil {
			return commands.Exitf(inv, 1, "skill list: %s", err)
		}
		if text != "" {
			fmt.Fprintln(inv.Stdout, text)
		}
		return nil
	case "disable", "enable":
		if d.Skill.SetDisabled == nil {
			return commands.Exitf(inv, 1, "skill %s: 此端无设备包管理（host 端命令）", sub)
		}
		if len(rest) != 1 {
			return commands.Exitf(inv, 2, "usage: skill %s <name>", sub)
		}
		if err := d.Skill.SetDisabled(ctx, sid, rest[0], sub == "disable"); err != nil {
			return commands.Exitf(inv, 1, "skill %s: %s", sub, err)
		}
		fmt.Fprintf(inv.Stdout, "%s %s\n", rest[0], map[bool]string{true: "disabled", false: "enabled"}[sub == "disable"])
		return nil
	case "remove":
		if d.Skill.Remove == nil {
			return commands.Exitf(inv, 1, "skill remove: 此端无设备包管理（host 端命令）")
		}
		if len(rest) != 1 {
			return commands.Exitf(inv, 2, "usage: skill remove <name>")
		}
		if err := d.Skill.Remove(ctx, sid, rest[0]); err != nil {
			return commands.Exitf(inv, 1, "skill remove: %s", err)
		}
		fmt.Fprintf(inv.Stdout, "%s removed\n", rest[0])
		return nil
	default:
		return commands.Exitf(inv, 2, "skill: 未知子命令 %q（search|load|download|list|disable|enable|remove）", sub)
	}
}

func (d PlatformDeps) cmdSendUser(ctx context.Context, inv *commands.Invocation) error {
	if helpRequested(inv.Args) {
		fmt.Fprintln(inv.Stdout, sendUserHelp)
		return nil
	}
	if len(inv.Args) == 0 {
		return commands.Exitf(inv, 2, "usage: send_user <消息...>")
	}
	if d.SendUser == nil {
		return commands.Exitf(inv, 1, "send_user: 此端未接通知通道")
	}
	if err := d.SendUser(ctx, SessionFromContext(ctx), strings.Join(inv.Args, " ")); err != nil {
		return commands.Exitf(inv, 1, "send_user: %s", err)
	}
	fmt.Fprintln(inv.Stdout, "sent")
	return nil
}
