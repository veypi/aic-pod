package vsh

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/veypi/vsh/commands"
)

// PlatformDeps 平台命令依赖（cmds.go）。零值可用：未注入的能力降级为可读
// 报错（命令存在、help 自答、执行提示该端不可用）。Tasks/RunBG 由
// NewEngine 自动接线，调用方无需填。
type PlatformDeps struct {
	Tasks *TaskTable
	// RunBG bg run 的执行体（引擎注入；签名：会话键、脚本、workdir、日志路径、
	// 日志 writer）。workdir = 调用方会话当前 cwd（2026-09-24 实测修复：bg 不再
	// 固定回落 HOME——相对路径写在 bg 里与前台一致）。
	RunBG func(ctx context.Context, sessionKey, script, workdir, logPath string, log io.Writer) (int, error)
	// Grant 发起授权申请（sessionKey=调用会话；domain: fs/net/ssh/cmd；
	// target: 路径/host:port/命令名；permanent=true 落盘永久生效——仅 host
	// 支持，cloud 无 permanent 档（D6）应拒绝）。返回给用户的可读结果文案；
	// 拒绝/失败返回 error。审批在工具层完成（脚本含字面 grant → 恒 4 级，
	// analyze GrantRequests），此处只执行授权动作本身。
	Grant func(ctx context.Context, sessionKey, domain, target string, permanent bool) (string, error)
	// GrantStatus 返回各域授权姿态与规则表（grant status——只读，不升档
	// 不审批）。nil = 降级报错。
	GrantStatus func(ctx context.Context, sessionKey string) (string, error)
	// ListHosts 返回预格式化的主机表文本（表格形态由平台定——cloud 给
	// Markdown 表；host 端通常 nil 降级）。sessionKey=调用会话。
	ListHosts func(ctx context.Context, sessionKey string) (string, error)
	// SendUser 给用户发消息（通知通道）。sessionKey=调用会话。
	SendUser func(ctx context.Context, sessionKey, message string) error
}

// HostInfo 主机摘要（预格式化提供方不再需要本结构——保留给未来结构化场景）。
type HostInfo struct {
	ID     string
	Name   string
	OS     string
	Online bool
}

// RegisterPlatformCommands 注册平台命令：commands / bg / grant / list_hosts /
// send_user。全部自带 help 文本（--help/-h 或无参数子命令自答）。
func RegisterPlatformCommands(reg *commands.Registry, deps PlatformDeps) error {
	for _, cmd := range []commands.Command{
		commands.DefineCommand("commands", cmdCommands),
		commands.DefineCommand("bg", deps.cmdBG),
		commands.DefineCommand("grant", deps.cmdGrant),
		commands.DefineCommand("list_hosts", deps.cmdListHosts),
		commands.DefineCommand("send_user", deps.cmdSendUser),
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

// cmdCommands 列出注册表全部命令（发现入口：exec 工具描述引导 commands +
// <cmd> --help）。
func cmdCommands(ctx context.Context, inv *commands.Invocation) error {
	if helpRequested(inv.Args) {
		fmt.Fprintln(inv.Stdout, "usage: commands — 列出当前环境可用的全部命令（用 <cmd> --help 查用法）")
		return nil
	}
	if inv.GetRegisteredCommands == nil {
		return commands.Exitf(inv, 1, "commands: registry unavailable")
	}
	for _, name := range inv.GetRegisteredCommands() {
		fmt.Fprintln(inv.Stdout, name)
	}
	return nil
}

const bgHelp = `usage:
  bg run <script...>      后台执行脚本（墙钟 30min，到期退出码 124）
  bg list                 列出后台任务
  bg wait <id> [秒]       等待任务结束并输出其结果
  bg output <id>          输出任务迄今捕获的内容（不等待）
  bg kill <id>            终止任务`

func (d PlatformDeps) cmdBG(ctx context.Context, inv *commands.Invocation) error {
	if len(inv.Args) == 0 || helpRequested(inv.Args) {
		fmt.Fprintln(inv.Stdout, bgHelp)
		return nil
	}
	if d.Tasks == nil {
		return commands.Exitf(inv, 1, "bg: task table unavailable on this endpoint")
	}
	switch inv.Args[0] {
	case "run":
		if len(inv.Args) < 2 {
			return commands.Exitf(inv, 2, "usage: bg run <script...>")
		}
		if d.RunBG == nil {
			return commands.Exitf(inv, 1, "bg: run unavailable on this endpoint")
		}
		script := strings.Join(inv.Args[1:], " ")
		sessionKey := inv.Env["AIC_VSH_SESSION"]
		cwd := ""
		if inv.FS != nil {
			cwd = inv.FS.Getwd()
		}
		task, err := d.Tasks.Start(script, "", OwnerFromContext(ctx), func(ctx context.Context, log io.Writer) (int, error) {
			return d.RunBG(ctx, sessionKey, script, cwd, "", log)
		}, nil)
		if err != nil {
			return commands.Exitf(inv, 1, "bg: %s", err)
		}
		fmt.Fprintf(inv.Stdout, "%s\n", task.ID)
		return nil
	case "list":
		tasks := d.Tasks.List()
		if len(tasks) == 0 {
			fmt.Fprintln(inv.Stdout, "(no background tasks)")
			return nil
		}
		for _, t := range tasks {
			fmt.Fprintf(inv.Stdout, "%s\t%s\texit=%d\t%s\n", t.ID, t.Status, t.ExitCode, t.Command)
		}
		return nil
	case "wait":
		if len(inv.Args) < 2 {
			return commands.Exitf(inv, 2, "usage: bg wait <id> [秒]")
		}
		wait := time.Duration(0)
		if len(inv.Args) >= 3 {
			sec, err := strconv.Atoi(inv.Args[2])
			if err != nil {
				return commands.Exitf(inv, 2, "bg wait: invalid seconds %q", inv.Args[2])
			}
			wait = time.Duration(sec) * time.Second
		} else {
			wait = 30 * time.Minute // 缺省等到墙钟
		}
		task, err := d.Tasks.Wait(ctx, inv.Args[1], wait)
		if err != nil {
			return commands.Exitf(inv, 1, "%s", err)
		}
		if out, oerr := d.Tasks.Output(inv.Args[1]); oerr == nil && out != "" {
			fmt.Fprint(inv.Stdout, out)
		}
		fmt.Fprintf(inv.Stdout, "%s\t%s\texit=%d\n", task.ID, task.Status, task.ExitCode)
		if task.Status == "running" {
			return nil // 等待超时但任务仍在跑：退出码 0、状态透出
		}
		if task.ExitCode != 0 {
			return &commands.ExitError{Code: task.ExitCode}
		}
		return nil
	case "output":
		if len(inv.Args) < 2 {
			return commands.Exitf(inv, 2, "usage: bg output <id>")
		}
		out, err := d.Tasks.Output(inv.Args[1])
		if err != nil {
			return commands.Exitf(inv, 1, "%s", err)
		}
		fmt.Fprint(inv.Stdout, out)
		return nil
	case "kill":
		if len(inv.Args) < 2 {
			return commands.Exitf(inv, 2, "usage: bg kill <id>")
		}
		if err := d.Tasks.Kill(inv.Args[1]); err != nil {
			return commands.Exitf(inv, 1, "%s", err)
		}
		fmt.Fprintf(inv.Stdout, "%s killed\n", inv.Args[1])
		return nil
	default:
		return commands.Exitf(inv, 2, "bg: unknown subcommand %q\n%s", inv.Args[0], bgHelp)
	}
}

const grantHelp = `usage:
  grant status                          查看各域授权姿态与规则表（只读）
  grant fs <路径> [--permanent]        申请文件访问（默认会话级临时授权；--permanent 落盘永久生效）
  grant net <host:port> [--permanent]  申请网络目标访问
  grant ssh <host:port> [--permanent]  申请 SSH 目标访问（host）
  grant cmd <命令名> [--permanent]     申请原生命令（host；授予解释器 = 授予该进程一切能力）`

func (d PlatformDeps) cmdGrant(ctx context.Context, inv *commands.Invocation) error {
	if len(inv.Args) == 0 || helpRequested(inv.Args) {
		fmt.Fprintln(inv.Stdout, grantHelp)
		return nil
	}
	// status 只读查询（analyze 只收两参字面 grant，本分支不触 4 级预检）。
	if inv.Args[0] == "status" {
		if d.GrantStatus == nil {
			return commands.Exitf(inv, 1, "grant: 此端未接状态查询")
		}
		text, err := d.GrantStatus(ctx, inv.Env["AIC_VSH_SESSION"])
		if err != nil {
			return commands.Exitf(inv, 1, "grant status: %s", err)
		}
		fmt.Fprintln(inv.Stdout, text)
		return nil
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
	msg, err := d.Grant(ctx, inv.Env["AIC_VSH_SESSION"], domain, target, permanent)
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
	text, err := d.ListHosts(ctx, inv.Env["AIC_VSH_SESSION"])
	if err != nil {
		return commands.Exitf(inv, 1, "list_hosts: %s", err)
	}
	fmt.Fprintln(inv.Stdout, text)
	return nil
}

const sendUserHelp = `usage: send_user <消息...> — 给用户发一条通知消息`

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
	if err := d.SendUser(ctx, inv.Env["AIC_VSH_SESSION"], strings.Join(inv.Args, " ")); err != nil {
		return commands.Exitf(inv, 1, "send_user: %s", err)
	}
	fmt.Fprintln(inv.Stdout, "sent")
	return nil
}
