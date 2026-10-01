package skillrun

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/veypi/vbox"
	"github.com/veypi/vsh/commands"

	vshglue "github.com/veypi/aic-pod/libs/vsh"
)

// rootCommand 构造包根命令（包名）：argv/stdin 全量透传默认 provider
// （providers[0]）；子命令与 --help 由包 CLI 自行实现。禁用 = 保留注册
// 但显式失败（不回落同名系统程序——包命令永不走 native fallback 之外
// 的第二通道）。service 类 provider 经 P0b 的懒启动 + skillproc 通道。
func (r *Registry) rootCommand(pkg *Package) commands.Command {
	return commands.DefineCommand(pkg.Name, func(ctx context.Context, inv *commands.Invocation) error {
		if pkg.Disabled() {
			return commands.Exitf(inv, 126, "%s: skill package disabled（skill 包已禁用，不回落系统程序）", pkg.Name)
		}
		p := pkg.Manifest.Default()
		switch p.Kind {
		case KindProcess:
			return r.runProcess(ctx, inv, pkg, p)
		default:
			return r.invokeService(ctx, inv, pkg, p)
		}
	})
}

// runProcess process 类调用：每次调用独立 vbox 沙箱进程（与 native 命令同一
// 沙箱派生——策略快照按调用身份当次取），stdio 直通引擎管道，ctx 取消即杀
// 进程组（vbox 受管取消）。
//
// v6 P5 确保机制（最薄）：包命令被调用时若 manifest 含 service 类 provider，
// 先全部确保懒启动（ensureService），并把 socket 路径注入进程 env——单个
// service 时同时给 SKILLPROC_SOCKET（browser/hello 同款约定），每个 service
// 恒给 SKILLPROC_SOCKET_<ID 大写>。process provider 经该 socket 拨号 svc
// （如 browser CLI 的无状态转发器）。
func (r *Registry) runProcess(ctx context.Context, inv *commands.Invocation, pkg *Package, p Provider) error {
	env, err := r.ensureServices(ctx, pkg)
	if err != nil {
		return commands.Exitf(inv, 1, "%s: %s", pkg.Name, err)
	}
	entry := filepath.Join(pkg.Dir, filepath.FromSlash(p.Entry))
	workdir := inv.Cwd
	if r.deps.Workdir != nil {
		workdir = r.deps.Workdir(inv.Cwd)
	}
	var pol vshglue.NativePolicy
	if r.deps.Policy != nil {
		pol = r.deps.Policy(ctx, workdir, pkg.Name)
	}
	argv := append([]string{entry}, p.Args...)
	argv = append(argv, inv.Args...)
	code, err := r.deps.Manager.RunProcess(ctx, vbox.StartOptions{
		Workdir:    workdir,
		Exec:       argv,
		Env:        env,
		NoSandbox:  pol.NoSandbox,
		WriteRoots: pol.WriteRoots,
		DenyPaths:  pol.DenyPaths,
		FSRules:    pol.FSRules,
		WritePaths: pol.WritePaths,
		FsOpen:     pol.FsOpen,
		NetOpen:    pol.NetOpen,
		NetDeny:    vshglue.ToVboxEntries(pol.NetDeny),
		NetAllow:   vshglue.ToVboxEntries(pol.NetAllow),
	}, inv.Stdin, inv.Stdout, inv.Stderr)
	if err != nil {
		return commands.Exitf(inv, exitCodeOr(code, 1), "%s: %s", pkg.Name, err)
	}
	if code != 0 {
		return &commands.ExitError{Code: code}
	}
	return nil
}

// ensureServices 确保包全部 service 类 provider 已懒启动，返回注入 process
// provider 的 env（socket 路径；无 service 时为 nil）。
func (r *Registry) ensureServices(ctx context.Context, pkg *Package) ([]string, error) {
	var sockets []string
	for _, p := range pkg.Manifest.Providers {
		if p.Kind != KindService {
			continue
		}
		inst, err := r.ensureService(ctx, pkg, p)
		if err != nil {
			return nil, err
		}
		sockets = append(sockets, "SKILLPROC_SOCKET_"+strings.ToUpper(p.ID)+"="+inst.socket)
	}
	if len(sockets) == 1 {
		// 单 service 包：无后缀约定键（SKILLPROC_SOCKET）。
		_, v, _ := strings.Cut(sockets[0], "=")
		sockets = append(sockets, socketEnv+"="+v)
	}
	return sockets, nil
}

// exitCodeOr 进程错误时的 exit code 兜底（与 native.go 同语义）。
func exitCodeOr(code, fallback int) int {
	if code != 0 {
		return code
	}
	return fallback
}
