package host

// Blender 内置服务（aic-skills/blender 包的 cli/ 维度）：vtools/tcp 工具通道
// + 生命周期动词 doctor/launch/install。分级报错是产品功能，不是异常——
// blender_not_installed → blender_not_running → addon_not_connected，
// 每级都给出 AI/用户可直接行动的指引。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/execution"
	"github.com/veypi/aic-pod/libs/vtools"
	"github.com/veypi/vsh/commands"
)

const blenderInstallGuide = `Blender 未安装。安装方式：
  macOS:   brew install --cask blender
  Windows: winget install BlenderFoundation.Blender
  Linux:   发行版包管理器（apt/dnf install blender）或 https://www.blender.org/download/
装在非标准位置时设 AIC_BLENDER_EXE 指向可执行文件。`

func (c *Client) blenderPack() (*packManifest, error) {
	packs, err := builtinPacks()
	if err != nil {
		return nil, err
	}
	m := packs["blender"]
	if m == nil || m.Lifecycle == nil {
		return nil, fmt.Errorf("blender pack 未内嵌或缺 lifecycle 清单")
	}
	return m, nil
}

// blenderAddr 生效的 addon 地址：清单 url，AIC_BLENDER_PORT 可覆盖端口
// （与 addon 侧同读一个 env，launch 拉起时透传）。
func blenderAddr(m *packManifest) string {
	addr := strings.TrimPrefix(m.Service.URL, "tcp://")
	if lc := m.Lifecycle; lc != nil && lc.PortEnv != "" {
		if v := os.Getenv(lc.PortEnv); v != "" {
			host, _, err := net.SplitHostPort(addr)
			if err == nil {
				addr = net.JoinHostPort(host, v)
			}
		}
	}
	return addr
}

// blenderProvider TCP 适配 + 错误翻译：拨号失败 = Blender 没在跑（分级 L2）。
// 连接地址按首次调用解析（AIC_BLENDER_PORT 覆盖生效时机与 addon 一致）。
type blenderProvider struct {
	addr  func() string
	once  sync.Once
	inner *vtools.TCPProvider
	err   error
}

func newBlenderProvider(m *packManifest) *blenderProvider {
	return &blenderProvider{addr: func() string { return blenderAddr(m) }}
}

// Addr 返回 addon 端点（vtools.AddrProvider）：登记层据此判定已登记服务是不是
// 同一个 TCP 端点（换型/换址即摘除重登记）。
func (b *blenderProvider) Addr() string { return b.addr() }

func (b *blenderProvider) provider() (vtools.Provider, error) {
	b.once.Do(func() {
		b.inner, b.err = vtools.NewTCP(b.addr())
	})
	if b.err != nil {
		return nil, b.err
	}
	return b.inner, nil
}

func blenderTranslateErr(err error) error {
	if err == nil {
		return nil
	}
	var ne *net.OpError
	if errors.As(err, &ne) {
		return fmt.Errorf("blender_not_running：addon 未连接（Blender 没在跑或 addon 未挂载）。agent 路径：blender launch；人工路径：启动 Blender 后用 blender install 安装 addon 并启用")
	}
	return err
}

func (b *blenderProvider) Tools(ctx context.Context) ([]vtools.Tool, error) {
	p, err := b.provider()
	if err != nil {
		return nil, err
	}
	tools, err := p.Tools(ctx)
	return tools, blenderTranslateErr(err)
}

func (b *blenderProvider) Call(ctx context.Context, tool string, args map[string]any) (any, error) {
	p, err := b.provider()
	if err != nil {
		return nil, err
	}
	result, err := p.Call(ctx, tool, args)
	return result, blenderTranslateErr(err)
}

// registerBuiltinPacks 注册内嵌包里的非 MCP 服务（当前 = blender 的 TCP 通道
// + 生命周期动词）。所有者 mcp.servers.<alias> 同名配置整体替换内置项。
// 单包失败只记日志跳过（与 syncServiceRegistrations 同口径）：内嵌包的 alias
// 撞名/kind 非法/地址无效都不能拖垮整机。alias 排序后注册——同 channel
// 撞名时先到先得，排序保证结果与 map 迭代顺序无关。
func (c *Client) registerBuiltinPacks() {
	packs, err := builtinPacks()
	if err != nil {
		c.logf("builtin packs unavailable（内嵌服务不登记，pod 照常运行）: %v", err)
		return
	}
	aliases := make([]string, 0, len(packs))
	for alias := range packs {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	for _, alias := range aliases {
		m := packs[alias]
		if m.Service.Kind == "mcp-stdio" {
			continue // configureMCP 已覆盖
		}
		if c.ownerConfigured(alias) || c.services.Has(alias) {
			continue
		}
		if err := c.registerPackService(alias, m); err != nil {
			c.logf("%v", err)
		}
	}
}

// registerPackService 登记单个内嵌包的非 MCP 服务。
func (c *Client) registerPackService(alias string, m *packManifest) error {
	switch m.Service.Kind {
	case "tcp":
		var p vtools.Provider
		cmd := commands.Command(nil)
		// verbs 是 blender 包的私有扩展（契约已收窄，不是通用 manifest 维度）
		if len(m.Verbs) > 0 && alias == "blender" {
			p = newBlenderProvider(m)
			cmd = commands.DefineCommand(alias, c.blenderCommand(alias))
		} else {
			tcp, err := vtools.NewTCP(blenderAddr(m))
			if err != nil {
				return fmt.Errorf("pack %s: %w", alias, err)
			}
			p = tcp
		}
		var err error
		if cmd != nil {
			err = c.services.RegisterCustom(alias, p, cmd)
		} else {
			err = c.services.Register(alias, p)
		}
		if err != nil {
			return fmt.Errorf("pack %s: %w", alias, err)
		}
		return nil
	default:
		return fmt.Errorf("pack %s: unknown service kind %q", alias, m.Service.Kind)
	}
}

const blenderHelp = `usage:
  blender                        工具目录（addon 未连接时给分级指引）
  blender doctor                 三级体检：装了没 → 跑了没 → addon 连上没
  blender launch                 拉起 GUI Blender 并挂载 addon（boot 脚本，幂等）
  blender install                把 addon 写进 Blender addons 目录（人工启用路径）
  blender <tool> [flags]         工具调用（<tool> --help 看参数表）

工具面：ping / get_scene_info / get_object_info / execute_code / get_viewport_screenshot。
长渲染/批量几何走 exec 原生无头（grant cmd blender 后 blender -b --python …）。`

func (c *Client) blenderCommand(alias string) commands.CommandFunc {
	return func(ctx context.Context, inv *commands.Invocation) error {
		if len(inv.Args) > 0 {
			switch inv.Args[0] {
			case "doctor":
				return c.blenderDoctor(ctx, inv)
			case "launch":
				return c.blenderLaunch(ctx, inv)
			case "install":
				return c.blenderInstall(ctx, inv)
			case "--help", "-h":
				fmt.Fprintln(inv.Stdout, blenderHelp)
				return nil
			}
		}
		return c.services.RunTool(ctx, inv, alias)
	}
}

// ---- doctor ----

func (c *Client) blenderDoctor(ctx context.Context, inv *commands.Invocation) error {
	out := inv.Stdout
	m, err := c.blenderPack()
	if err != nil {
		return commands.Exitf(inv, 1, "blender doctor: %s", err)
	}
	// L1：装了没（env 覆盖也要存在才算数——写错的 AIC_BLENDER_EXE 在 doctor
	// 就暴露，而不是攒到 launch 才炸）
	exe := m.Lifecycle.findExecutable()
	if exe == "" {
		fmt.Fprintf(out, "L1 ✗ blender_not_installed\n%s\n", blenderInstallGuide)
		return &commands.ExitError{Code: 1}
	}
	if st, err := os.Stat(exe); err != nil || st.IsDir() {
		fmt.Fprintf(out, "L1 ✗ blender_not_installed（%s 不存在：检查 AIC_BLENDER_EXE）\n%s\n", exe, blenderInstallGuide)
		return &commands.ExitError{Code: 1}
	}
	fmt.Fprintf(out, "L1 ✓ blender: %s\n", exe)
	// L2：跑了没（端口通）
	addr := blenderAddr(m)
	conn, err := net.DialTimeout("tcp", addr, 1500*time.Millisecond)
	if err != nil {
		fmt.Fprintf(out, "L2 ✗ blender_not_running（%s 不通）\n处置：blender launch（agent 路径，幂等拉起）；或人工启动 Blender 并启用 addon\n", addr)
		return &commands.ExitError{Code: 1}
	}
	_ = conn.Close()
	fmt.Fprintf(out, "L2 ✓ addon 端口连通（%s）\n", addr)
	// L3：握手（addon 版本与协议对得上才数）
	tcp, err := vtools.NewTCP(addr)
	if err != nil {
		return commands.Exitf(inv, 1, "blender doctor: %s", err)
	}
	defer tcp.Close()
	ping, err := tcp.Call(ctx, "ping", map[string]any{})
	if err != nil {
		fmt.Fprintf(out, "L3 ✗ addon_not_connected：%s\n处置：blender launch 重挂 addon（pod 升级后 addon 可能已换版）\n", err)
		return &commands.ExitError{Code: 1}
	}
	raw, _ := json.Marshal(ping)
	fmt.Fprintf(out, "L3 ✓ addon 握手：%s\nkind=ok\n", raw)
	return nil
}

// ---- launch ----

func (c *Client) blenderLaunch(ctx context.Context, inv *commands.Invocation) error {
	if err := c.mcpGate(ctx, execution.SessionFromContext(ctx), "blender"); err != nil {
		return commands.Exitf(inv, 126, "%s", err)
	}
	out := inv.Stdout
	m, err := c.blenderPack()
	if err != nil {
		return commands.Exitf(inv, 1, "blender launch: %s", err)
	}
	lc := m.Lifecycle
	addr := blenderAddr(m)
	// 幂等：addon 已在服务就直接返回
	tcp, err := vtools.NewTCP(addr)
	if err == nil {
		if _, err := tcp.Call(ctx, "ping", map[string]any{}); err == nil {
			fmt.Fprintf(out, "blender 已在运行（%s）\n", addr)
			_ = tcp.Close()
			return nil
		}
		_ = tcp.Close()
	}
	exe := lc.findExecutable()
	if exe == "" {
		fmt.Fprintf(out, "blender_not_installed\n%s\n", blenderInstallGuide)
		return &commands.ExitError{Code: 1}
	}
	if st, err := os.Stat(exe); err != nil || st.IsDir() {
		fmt.Fprintf(out, "blender_not_installed（%s 不存在：检查 AIC_BLENDER_EXE）\n%s\n", exe, blenderInstallGuide)
		return &commands.ExitError{Code: 1}
	}
	// 挂载面：addon + boot 脚本写到 pod 状态目录（不动用户 Blender 配置）
	stateDir, err := cfg.StateDir()
	if err != nil {
		return commands.Exitf(inv, 1, "blender launch: %s", err)
	}
	dir := filepath.Join(stateDir, "blender")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return commands.Exitf(inv, 1, "blender launch: %s", err)
	}
	addon, err := packAsset("blender", lc.BootAsset)
	if err != nil {
		return commands.Exitf(inv, 1, "blender launch: addon 资产缺失: %s", err)
	}
	addonPath := filepath.Join(dir, "aic_blender_bridge.py")
	if err := os.WriteFile(addonPath, addon, 0o644); err != nil {
		return commands.Exitf(inv, 1, "blender launch: %s", err)
	}
	boot := "import runpy\nns = runpy.run_path(%q)\nns[\"start\"]()\n"
	bootPath := filepath.Join(dir, "boot.py")
	if err := os.WriteFile(bootPath, []byte(fmt.Sprintf(boot, addonPath)), 0o644); err != nil {
		return commands.Exitf(inv, 1, "blender launch: %s", err)
	}
	// 拉起 GUI（桌面应用，与 browser/cua 同口径不经沙箱；detached，不随 pod 退出）
	cmd := spawnDetached(exe, "--python", bootPath)
	if lc.PortEnv != "" {
		if v := os.Getenv(lc.PortEnv); v != "" {
			cmd.Env = append(os.Environ(), lc.PortEnv+"="+v)
		}
	}
	status := map[string]any{"exe": exe, "boot": bootPath, "addr": addr, "at": time.Now().Format(time.RFC3339)}
	statusFile := filepath.Join(dir, "launch-status.json")
	writeStatus := func() {
		raw, _ := json.MarshalIndent(status, "", "  ")
		_ = os.WriteFile(statusFile, raw, 0o644)
	}
	if err := cmd.Start(); err != nil {
		status["error"] = err.Error()
		writeStatus()
		return commands.Exitf(inv, 1, "blender launch: 启动失败: %s（详见 %s）", err, statusFile)
	}
	status["pid"] = cmd.Process.Pid
	_ = cmd.Process.Release()
	// 轮询握手（GUI 冷启动 + addon 挂载，给足 20s）：ping 通了才算数——
	// 只 dial 端口会被 9866 上的无关占用者骗出假成功。
	probe, err := vtools.NewTCP(addr)
	if err != nil {
		return commands.Exitf(inv, 1, "blender launch: %s", err)
	}
	defer probe.Close()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := probe.Call(ctx, "ping", map[string]any{}); err == nil {
			status["ok"] = true
			writeStatus()
			fmt.Fprintf(out, "blender 已拉起（pid %d，addon @ %s）\n", status["pid"], addr)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	status["error"] = "timeout waiting for " + addr
	writeStatus()
	fmt.Fprintf(out, "blender 已启动但 addon 20s 未就绪（addon 挂载失败？详见 %s）\n", statusFile)
	return &commands.ExitError{Code: 1}
}

// ---- install ----

func (c *Client) blenderInstall(ctx context.Context, inv *commands.Invocation) error {
	if err := c.mcpGate(ctx, execution.SessionFromContext(ctx), "blender"); err != nil {
		return commands.Exitf(inv, 126, "%s", err)
	}
	out := inv.Stdout
	m, err := c.blenderPack()
	if err != nil {
		return commands.Exitf(inv, 1, "blender install: %s", err)
	}
	dirs := m.Lifecycle.AddonsDirs[runtime.GOOS]
	if dirs == "" {
		return commands.Exitf(inv, 1, "blender install: 不支持的平台 %s", runtime.GOOS)
	}
	home, _ := os.UserHomeDir()
	base := expandPackVars(dirs, home, "")
	// Blender addons 按版本分目录：<base>/<版本>/scripts/addons/
	matches, _ := filepath.Glob(filepath.Join(base, "*", "scripts", "addons"))
	if len(matches) == 0 {
		return commands.Exitf(inv, 1, "blender install: 未找到 Blender addons 目录（%s 下无版本目录）——先跑一次 Blender 生成配置，或直接用 blender launch（boot 挂载，免安装）", base)
	}
	addon, err := packAsset("blender", m.Lifecycle.BootAsset)
	if err != nil {
		return commands.Exitf(inv, 1, "blender install: addon 资产缺失: %s", err)
	}
	var written []string
	for _, dir := range matches {
		target := filepath.Join(dir, "aic_blender_bridge.py")
		if err := os.WriteFile(target, addon, 0o644); err != nil {
			return commands.Exitf(inv, 1, "blender install: 写 %s 失败: %s", target, err)
		}
		written = append(written, target)
	}
	fmt.Fprintf(out, "addon 已写入：\n  %s\n启用：Blender → Edit ▸ Preferences ▸ Add-ons → 搜 \"AIC\" → 勾选 AIC Blender Bridge（启用即监听 127.0.0.1:9866）\n", strings.Join(written, "\n  "))
	return nil
}
