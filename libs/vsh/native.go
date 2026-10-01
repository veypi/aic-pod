package vsh

import (
	"context"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strings"
	"sync"

	"github.com/veypi/aic-pod/libs/netauth"
	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/vbox"
	"github.com/veypi/vsh/commands"
)

// NativePolicy 是原生命令一次执行所需的策略快照（每次调用取当次值——
// grant/cfg 动态生效）。字段对齐 vbox.StartOptions。
// hosts-vsh-redesign：数字等级已删除——沙箱 profile 一律由 rules 派生
// （WriteRoots/DenyPaths/FSRules/…），免沙箱只来自可信 ctx 的
// NoSandbox（不再读脚本可修改的 env）。
type NativePolicy struct {
	WriteRoots []string // 追加可写根（cfg fs_allow + grant fs）
	DenyPaths  []string // 预展开 deny 模式
	WritePaths []string // 可写 glob
	FSRules    *vbox.FSRuleSet
	FsOpen     bool
	NetOpen    bool
	NetDeny    []netauth.Entry
	NetAllow   []netauth.Entry
	// NoSandbox 免沙箱标记（显式 nosandbox，发送前审批下发）。
	NoSandbox bool
}

// NativeDeps 原生命令包装器依赖。
type NativeDeps struct {
	Manager *vbox.Manager
	// Policy 当次策略快照源（按调用取——workdir 为 OS 原生态、name 为命令名，
	// 供工作区元数据保护行派生；会话/免沙箱经可信 ctx 透传：
	// SessionFromContext / NoSandboxFromContext，由引擎注入）。
	Policy func(ctx context.Context, workdir, name string) NativePolicy
	// LookPath name → 真实二进制路径；nil = exec.LookPath。
	LookPath func(name string) (string, error)
	// Workdir 进程 cwd（空 = inv.Cwd 直通）。inv.Cwd 是引擎规范形（win =
	// /c/…）——host 装配侧必须经 proto.HostPathToOS 转原生态再启动进程。
	// 返回空串 = 继承 pod cwd。
	Workdir func(invCwd string) string
	// SessionAllow 会话级命令规则（grant cmd 会话授权；host 接线
	// Client.execGrants）。nil = 无会话授权。
	SessionAllow func(session, name string) bool
}

// NativeRegistry 是 host 原生命令的规则门与适配器（hosts-vsh-redesign §2.1：
// 原生程序不逐个注册——Registry 未命中时由 OpenLookup 兜底合成，执行期按
// 命令规则检查（deny 优先 → open 姿态 → exec_allow 白名单 → 会话 grant），
// 实际进程由 vbox OS 沙箱约束）。
type NativeRegistry struct {
	deps    NativeDeps
	mu      sync.RWMutex
	allowed map[string]string // name → binary 路径（空 = 惰性 LookPath）
	open    bool              // exec_policy: open——规则全放（deny 优先）
	deny    map[string]bool   // exec_deny（含 "*" 全禁）
}

func NewNativeRegistry(deps NativeDeps) *NativeRegistry {
	if deps.LookPath == nil {
		deps.LookPath = exec.LookPath
	}
	return &NativeRegistry{deps: deps, allowed: map[string]string{}}
}

// Seed 批量登记规则白名单（cfg exec_allow 种子——规则数据，不是注册动作）。
func (n *NativeRegistry) Seed(names ...string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name != "" {
			n.allowed[name] = ""
		}
	}
}

// SetPolicy 同步 exec 域规则（exec_policy + exec_deny；cfg 加载/Reconcile
// 调用）。deny 对全部原生命令执行期即时生效。
func (n *NativeRegistry) SetPolicy(open bool, deny []string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.open = open
	n.deny = map[string]bool{}
	for _, d := range deny {
		d = strings.TrimSpace(d)
		if d != "" {
			n.deny[d] = true
		}
	}
}

// IsAllowed 报告命令是否放行：deny 优先（"*" 全禁）；open 姿态全放；
// 否则看白名单或会话 grant（session 为空时不查会话授权）。
func (n *NativeRegistry) IsAllowed(session, name string) bool {
	n.mu.RLock()
	deny := n.deny
	open := n.open
	_, whitelisted := n.allowed[name]
	n.mu.RUnlock()
	if deny["*"] || deny[name] {
		return false
	}
	if open || whitelisted {
		return true
	}
	if session != "" && n.deps.SessionAllow != nil {
		return n.deps.SessionAllow(session, name)
	}
	return false
}

// OpenLookup 引擎解析兜底：Registry 未命中时合成原生命令——任意良名都合成
// （规则检查在执行期，IsAllowed 拒绝时返回权限错误而非 127；二进制不存在
// 才 127）。这不是免检查通道：原生 fallback 受命令规则与进程沙箱约束。
//
// 含 "/" 的显式程序路径（引擎二进制分支）同样经此合成：仅当目标文件存在
// 且可执行时命中（unix 要求执行位；windows 只看存在），执行走同一条规则 +
// OS 沙箱通道，规则检查用 basename 口径。含空格/制表符或反斜杠的名字仍拒绝
// （避免绕过 shell 引号语义）。
func (n *NativeRegistry) OpenLookup(name string) (commands.Command, bool) {
	if name == "" || strings.ContainsAny(name, "\\ \t") {
		return nil, false
	}
	if strings.Contains(name, "/") {
		if !explicitPathExecutable(name) {
			return nil, false
		}
		return n.commandPath(name), true
	}
	return n.command(name), true
}

// explicitPathExecutable 报告显式程序路径（引擎规范形）是否可原生执行。
func explicitPathExecutable(name string) bool {
	info, err := os.Stat(proto.HostPathToOS(name))
	if err != nil || info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode()&0o111 != 0
}

// command 构造单命令包装器：stdio 接引擎管道，子进程由 vbox 的 OS
// 沙箱兜底（Seatbelt/bwrap/受限令牌，per-call 按当次规则生成，fail-closed）。
func (n *NativeRegistry) command(name string) commands.Command {
	return commands.DefineCommand(name, func(ctx context.Context, inv *commands.Invocation) error {
		session := SessionFromContext(ctx)
		if !n.IsAllowed(session, name) {
			return commands.Exitf(inv, 126, "permission_denied: %s: command denied by exec rules（grant cmd %s 申请）", name, name)
		}
		if n.deps.Manager == nil {
			return commands.Exitf(inv, 1, "%s: native process manager unavailable", name)
		}
		bin, err := n.deps.LookPath(name)
		if err != nil {
			return commands.Exitf(inv, 127, "%s: binary not found: %s", name, err)
		}
		workdir := inv.Cwd
		if n.deps.Workdir != nil {
			workdir = n.deps.Workdir(inv.Cwd)
		}
		var pol NativePolicy
		if n.deps.Policy != nil {
			pol = n.deps.Policy(ctx, workdir, name)
		}
		code, err := n.deps.Manager.RunProcess(ctx, vbox.StartOptions{
			Workdir:    workdir,
			Exec:       append([]string{bin}, inv.Args...),
			NoSandbox:  pol.NoSandbox,
			WriteRoots: pol.WriteRoots,
			DenyPaths:  pol.DenyPaths,
			FSRules:    pol.FSRules,
			WritePaths: pol.WritePaths,
			FsOpen:     pol.FsOpen,
			NetOpen:    pol.NetOpen,
			NetDeny:    ToVboxEntries(pol.NetDeny),
			NetAllow:   ToVboxEntries(pol.NetAllow),
		}, inv.Stdin, inv.Stdout, inv.Stderr)
		if err != nil {
			return commands.Exitf(inv, exitCodeOr(code, 1), "%s: %s", name, err)
		}
		if code != 0 {
			return &commands.ExitError{Code: code}
		}
		return nil
	})
}

// commandPath 构造显式程序路径（引擎规范形，含 "/"）的原生命令包装器。
// 与 command 同一条通道：命令规则（basename 口径）、执行期策略快照、OS
// 沙箱；bin 直取给定路径（不做 LookPath），文件须存在。
func (n *NativeRegistry) commandPath(filePath string) commands.Command {
	return commands.DefineCommand(filePath, func(ctx context.Context, inv *commands.Invocation) error {
		session := SessionFromContext(ctx)
		name := path.Base(filePath)
		if !n.IsAllowed(session, name) {
			return commands.Exitf(inv, 126, "permission_denied: %s: command denied by exec rules（grant cmd %s 申请）", filePath, name)
		}
		if n.deps.Manager == nil {
			return commands.Exitf(inv, 1, "%s: native process manager unavailable", filePath)
		}
		bin := proto.HostPathToOS(filePath)
		if info, err := os.Stat(bin); err != nil || info.IsDir() {
			return commands.Exitf(inv, 127, "%s: No such file or directory", filePath)
		}
		workdir := inv.Cwd
		if n.deps.Workdir != nil {
			workdir = n.deps.Workdir(inv.Cwd)
		}
		var pol NativePolicy
		if n.deps.Policy != nil {
			pol = n.deps.Policy(ctx, workdir, name)
		}
		code, err := n.deps.Manager.RunProcess(ctx, vbox.StartOptions{
			Workdir:    workdir,
			Exec:       append([]string{bin}, inv.Args...),
			NoSandbox:  pol.NoSandbox,
			WriteRoots: pol.WriteRoots,
			DenyPaths:  pol.DenyPaths,
			FSRules:    pol.FSRules,
			WritePaths: pol.WritePaths,
			FsOpen:     pol.FsOpen,
			NetOpen:    pol.NetOpen,
			NetDeny:    ToVboxEntries(pol.NetDeny),
			NetAllow:   ToVboxEntries(pol.NetAllow),
		}, inv.Stdin, inv.Stdout, inv.Stderr)
		if err != nil {
			return commands.Exitf(inv, exitCodeOr(code, 1), "%s: %s", filePath, err)
		}
		if code != 0 {
			return &commands.ExitError{Code: code}
		}
		return nil
	})
}

// exitCodeOr 保持非零退出码，零值回退 fallback。

// ToVboxEntries netauth 目标快照转 vbox 形态（同构 {Host, Port}——
// 沙箱输入统一为 vbox.Entry）。
func ToVboxEntries(es []netauth.Entry) []vbox.Entry {
	if len(es) == 0 {
		return nil
	}
	out := make([]vbox.Entry, 0, len(es))
	for _, e := range es {
		out = append(out, vbox.Entry{Host: e.Host, Port: e.Port})
	}
	return out
}

func exitCodeOr(code, fallback int) int {
	if code != 0 {
		return code
	}
	return fallback
}
