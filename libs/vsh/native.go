package vsh

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"github.com/veypi/aic-pod/libs/exec_procs"
	"github.com/veypi/aic-pod/libs/netauth"
	"github.com/veypi/vsh/commands"
)

// NativePolicy 是原生命令一次执行所需的策略快照（每次调用取当次值——
// grant/cfg 动态生效）。字段对齐 exec_procs.StartOptions。
type NativePolicy struct {
	Level        int      // 当次授予等级（沙箱 profile 选择；0=read-only 兜底）
	WriteRoots   []string // 追加可写根（cfg fs_allow + grant fs）
	DenyPaths    []string // 预展开 deny 模式
	WritePaths   []string // 可写 glob
	SandboxRules []exec_procs.SandboxRule
	FsOpen       bool
	NetOpen      bool
	NetDeny      []netauth.Entry
	NetAllow     []netauth.Entry
	// NoSandbox 免沙箱标记（显式 nosandbox 经 Critical(4) 审批下发）。
	NoSandbox bool
}

// NativeDeps 原生命令包装器依赖。
type NativeDeps struct {
	Manager *exec_procs.Manager
	// Policy 当次策略快照源（按调用取——sid/level 经 inv.Env 透传：
	// AIC_VSH_SESSION / AIC_VSH_LEVEL，由引擎调用方注入）。
	Policy func(inv *commands.Invocation) NativePolicy
	// LookPath name → 真实二进制路径；nil = exec.LookPath。
	LookPath func(name string) (string, error)
	// Workdir 进程 cwd（空 = inv.Cwd 直通）。inv.Cwd 是引擎规范形（win =
	// /c/…）——host 装配侧必须经 proto.HostPathToOS 转原生态再启动进程。
	// 返回空串 = 继承 pod cwd。
	Workdir func(invCwd string) string
	// LogPath 输出落盘路径（可选；空 = 不落盘）。
	LogPath func() string
}

// NativeRegistry 是 host 原生命令白名单注册器（design §4.2：种子 =
// caps/exec_allow 声明，运行时 grant cmd 扩充；exec_policy: open 时
// 未注册名经 OpenLookup 兜底即时合成——原生边界统一收进 exec_procs
// OS 沙箱）。
type NativeRegistry struct {
	deps    NativeDeps
	mu      sync.RWMutex
	allowed map[string]string // name → binary 路径（空 = 惰性 LookPath）
	open    bool              // exec_policy: open——未注册名也放行（deny 优先）
	deny    map[string]bool   // exec_deny（含 "*" 全禁）
}

func NewNativeRegistry(deps NativeDeps) *NativeRegistry {
	if deps.LookPath == nil {
		deps.LookPath = exec.LookPath
	}
	return &NativeRegistry{deps: deps, allowed: map[string]string{}}
}

// Seed 批量登记白名单（caps/exec_allow 种子）。
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

// SetPolicy 同步 exec 域策略（exec_policy + exec_deny；cfg 加载/Reconcile
// 调用）。open=true 时未注册名经 OpenLookup 兜底放行——OS 沙箱仍是唯一
// 执行边界；deny 对全部原生命令（含已注册）执行期即时生效。
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

// Allow 运行时扩充（grant cmd 审批通过后调用）。
func (n *NativeRegistry) Allow(name string) { n.Seed(name) }

// IsAllowed 报告命令是否放行：deny 优先（"*" 全禁）；open 姿态全放；
// 否则看白名单。
func (n *NativeRegistry) IsAllowed(name string) bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.deny["*"] || n.deny[name] {
		return false
	}
	if n.open {
		return true
	}
	_, ok := n.allowed[name]
	return ok
}

// OpenLookup exec_policy: open 的引擎解析兜底：Registry 未命中且策略放行
// 时即时合成本地命令（执行体与注册命令同一包装器——exec_procs 沙箱兜底）。
// 白名单姿态下未注册名返回 false（继续走 PATH/hash 解析，最终 127）。
func (n *NativeRegistry) OpenLookup(name string) (commands.Command, bool) {
	if name == "" || strings.ContainsAny(name, "/\\ \t") {
		return nil, false
	}
	if !n.IsAllowed(name) {
		return nil, false
	}
	return n.command(name), true
}

// Names 返回白名单快照（排序交由调用方）。
func (n *NativeRegistry) Names() []string {
	n.mu.RLock()
	defer n.mu.RUnlock()
	out := make([]string, 0, len(n.allowed))
	for name := range n.allowed {
		out = append(out, name)
	}
	return out
}

// RegisterInto 把白名单内全部命令注册进引擎 Registry。每个命令 =
// 包装器：stdio 接引擎管道，子进程由 exec_procs 的 OS 沙箱兜底
// （Seatbelt/bwrap/受限令牌，per-call 按当次策略生成，fail-closed）。
func (n *NativeRegistry) RegisterInto(reg *commands.Registry) error {
	for _, name := range n.Names() {
		if err := reg.Register(n.command(name)); err != nil {
			return err
		}
	}
	return nil
}

// Register 注册单个命令（grant cmd 后即时生效）。
func (n *NativeRegistry) Register(reg *commands.Registry, name string) error {
	if !n.IsAllowed(name) {
		return fmt.Errorf("vsh glue: native command %q not in whitelist", name)
	}
	return reg.Register(n.command(name))
}

// command 构造单命令包装器。
func (n *NativeRegistry) command(name string) commands.Command {
	return commands.DefineCommand(name, func(ctx context.Context, inv *commands.Invocation) error {
		if !n.IsAllowed(name) {
			// 白名单在运行期被移除的兜底（正常路径：未注册即 127，到不了这里）。
			return commands.Exitf(inv, 127, "%s: command not granted（grant cmd %s 申请）", name, name)
		}
		n.mu.RLock()
		bin := n.allowed[name]
		n.mu.RUnlock()
		if bin == "" {
			var err error
			bin, err = n.deps.LookPath(name)
			if err != nil {
				return commands.Exitf(inv, 127, "%s: binary not found: %s", name, err)
			}
			n.mu.Lock()
			n.allowed[name] = bin
			n.mu.Unlock()
		}
		if n.deps.Manager == nil {
			return commands.Exitf(inv, 1, "%s: native process manager unavailable", name)
		}
		var pol NativePolicy
		if n.deps.Policy != nil {
			pol = n.deps.Policy(inv)
		}
		workdir := inv.Cwd
		if n.deps.Workdir != nil {
			workdir = n.deps.Workdir(inv.Cwd)
		}
		var logPath string
		if n.deps.LogPath != nil {
			logPath = n.deps.LogPath()
		}
		code, err := n.deps.Manager.RunProcess(ctx, exec_procs.StartOptions{
			Command:      name + " " + strings.Join(inv.Args, " "),
			LogPath:      logPath,
			Workdir:      workdir,
			Exec:         append([]string{bin}, inv.Args...),
			Level:        pol.Level,
			NoSandbox:    pol.NoSandbox,
			WriteRoots:   pol.WriteRoots,
			DenyPaths:    pol.DenyPaths,
			SandboxRules: pol.SandboxRules,
			WritePaths:   pol.WritePaths,
			FsOpen:       pol.FsOpen,
			NetOpen:      pol.NetOpen,
			NetDeny:      pol.NetDeny,
			NetAllow:     pol.NetAllow,
		}, inv.Stdout)
		if err != nil {
			return commands.Exitf(inv, exitCodeOr(code, 1), "%s: %s", name, err)
		}
		if code != 0 {
			return &commands.ExitError{Code: code}
		}
		return nil
	})
}

func exitCodeOr(code, fallback int) int {
	if code != 0 {
		return code
	}
	return fallback
}
