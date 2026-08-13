package browser

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/aic-pod/libs/vcore"
)

// run 分发子命令（§5.6.3 输入输出标准，agent-browser 基准）：
// 特化子命令（安全/文件交换/截断/本地实现）在下方 switch 显式处理；
// **其余子命令一律透传给 agent-browser CLI**（以 agent-browser 为唯一基准，
// 新命令自动可用）——vcore 只对透传结果做统一的输出收尾。
func (b *Browser) run(ctx context.Context, env *vcore.Env, sub string, args []string) (*vcore.Result, error) {
	r := &vcore.Result{Attrs: map[string]string{"action": "browser"}}
	var out string
	var err error

	switch sub {
	case "open":
		out, err = b.open(ctx, args)
	case "click", "eval", "get", "snapshot", "tab", "wait",
		"type", "fill", "press", "keyboard", "hover", "focus", "check", "uncheck",
		"select", "drag", "scroll", "scrollintoview", "is", "find", "mouse", "set",
		"dblclick", "back", "forward", "reload", "diff", "trace", "profiler",
		"record", "console", "errors", "highlight", "inspect", "clipboard", "pushstate",
		"batch", "session", "mcp", "skills", "auth", "plugin", "confirm", "deny",
		"react", "vitals", "dashboard", "install", "upgrade", "doctor", "profiles",
		"cookies", "storage", "route", "unroute", "har":
		// 云环境（Vars 非空）过滤调试端点泄露：get cdp-url 暴露本机 CDP 监听地址
		if sub == "get" && env.Vars != nil {
			for _, a := range args {
				if a == "cdp-url" {
					return nil, &proto.ExecError{Action: "browser",
						Reason: "get cdp-url is not available in cloud environment"}
				}
			}
		}
		out, err = b.execCLI(ctx, nil, append([]string{sub}, args...)...)
		out, err = b.postProcess(sub, out, r, err)
	case "close":
		// 与 agent-browser save/close 机制对齐：**任何 CLI 调用都会唤起 daemon**——
		// close 后的异步保存（markDirty 防抖）必然重启浏览器实例。
		// 因此：close 前同步冲刷脏 state（下次 open 恢复），close 后零 CLI 调用。
		b.mu.Lock()
		dirty := b.dirty
		if b.saveTimer != nil {
			b.saveTimer.Stop()
			b.saveTimer = nil
		}
		b.dirty = false
		b.mu.Unlock()
		if dirty {
			b.saveState(ctx)
		}
		// args 透传（close --all 关闭全部会话）
		out, err = b.execCLI(ctx, nil, append([]string{"close"}, args...)...)
		if err == nil {
			r.Attrs["closed"] = "true"
		}
	case "read":
		// 截断由 exec 统一层负责（procs.EnforceResultLimits，§2.5 512KB），子指令不做特化截断
		out, err = b.execCLI(ctx, nil, append([]string{"read"}, args...)...)
	case "network":
		out, err = b.network(ctx, env, args, r)
	case "download":
		return b.download(ctx, env, args)
	case "screenshot":
		return b.screenshot(ctx, env, args)
	case "pdf":
		return b.pdf(ctx, env, args)
	case "sleep":
		out, err = sleepAction(ctx, args)
	case "upload":
		return b.upload(ctx, env, args)
	default:
		// 透传：agent-browser 未知子命令由其自身报错（版本差异由 CLI 收敛）
		out, err = b.execCLI(ctx, nil, append([]string{sub}, args...)...)
	}

	if err != nil {
		return nil, &proto.ExecError{Action: "browser", Reason: fmt.Sprintf("%s: %s", sub, err)}
	}
	if out == "" {
		out = "(no output)"
	}
	r.Content = out
	b.applyExecAttrs(r) // §5.9：托管结果 attrs（path/rows/truncated/background/id）
	if sub != "sleep" && sub != "close" {
		b.markDirty()
	}
	return r, nil
}

// open 处理导航：仅 http/https；会话未存活且存在已保存 state 时自动加载（§5.6）。
func (b *Browser) open(ctx context.Context, args []string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("url is required")
	}
	if !strings.HasPrefix(args[0], "http://") && !strings.HasPrefix(args[0], "https://") {
		return "", fmt.Errorf("only http(s) urls are supported")
	}
	var flags []string
	if b.cfg.StatePath != "" && !b.sessionAlive(ctx) {
		if _, err := os.Stat(b.cfg.StatePath); err == nil {
			// 新 session 首次导航时自动加载已保存 state（§5.6 自动保存闭环）
			flags = append(flags, "--state", b.cfg.StatePath)
		}
	}
	out, err := b.execCLI(ctx, flags, append([]string{"open"}, args...)...)
	if err == nil {
		b.markDirty()
	}
	return out, err
}

// sessionAlive 检查浏览器会话是否存活。
func (b *Browser) sessionAlive(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := b.execCLI(ctx, nil, "session", "list")
	return err == nil && strings.Contains(out, b.cfg.Session)
}

// network 处理：子命令模式（route/unroute/request/har）与列表模式（requests）
// 一律透传 agent-browser；截断由 exec 统一层负责（procs.EnforceResultLimits，§2.5），
// 子指令不做特化截断。
func (b *Browser) network(ctx context.Context, env *vcore.Env, args []string, r *vcore.Result) (string, error) {
	sub := "requests"
	if len(args) > 0 && !strings.HasPrefix(args[0], "--") {
		sub = args[0] // route/unroute/request/har...
		args = args[1:]
	}
	// har stop [path]：导出文件走 VFS 沙箱（§5.9，与 download/pdf 同构）
	if sub == "har" && len(args) > 0 && args[0] == "stop" && len(args) > 1 {
		res, err := b.exportFile(ctx, env, "network har stop", args[1], r, "har", "✓ HAR saved to %s")
		if err != nil {
			return "", err
		}
		return res.Content, nil
	}
	return b.execCLI(ctx, nil, append([]string{"network", sub}, args...)...)
}

// pdf 保存页面为 PDF（§5.9）：目标路径走 VFS 沙箱——
// Roots 收容 + 云环境 $SESSION 约束；CLI 写临时文件后字节经 VFS 落盘。
func (b *Browser) pdf(ctx context.Context, env *vcore.Env, args []string) (*vcore.Result, error) {
	if len(args) < 1 {
		return nil, &proto.ExecError{Action: "browser", Reason: "pdf requires a path"}
	}
	res, err := b.exportFile(ctx, env, "pdf", args[0], &vcore.Result{Attrs: map[string]string{"action": "browser"}}, "pdf", "✓ Saved PDF to %s")
	if err != nil {
		return nil, err
	}
	return res, nil
}

// exportFile 通用导出：CLI 写临时文件 → 字节经 VFS 写目标路径（§5.9）。
// cliArgs 为 CLI 调用参数（不含输出路径），ext 为临时文件扩展名，
// msg 为成功文案（%s = VFS 路径）。路径约束：Roots 收容 + 云环境 $SESSION。
func (b *Browser) exportFile(ctx context.Context, env *vcore.Env, cliCmd, rawPath string, r *vcore.Result, ext, msg string) (*vcore.Result, error) {
	abs, err := env.Resolve(rawPath)
	if err != nil {
		return nil, &proto.ExecError{Action: "browser", Reason: fmt.Sprintf("%s: %s", cliCmd, err)}
	}
	if err := env.CheckPath("browser", abs); err != nil {
		return nil, err
	}
	// 云环境（Vars 非空）：导出产物限 $SESSION 根内（与 download 同语义，§5.6）
	if sess := env.Vars["$SESSION"]; sess != "" {
		if abs != sess && !strings.HasPrefix(abs, sess+"/") {
			return nil, &proto.ExecError{Action: "browser",
				Reason: fmt.Sprintf("%s: path outside $SESSION root: %s", cliCmd, abs)}
		}
	}
	if err := os.MkdirAll(b.cfg.TempDir, 0o700); err != nil {
		return nil, err
	}
	tmpPath := filepath.Join(b.cfg.TempDir, fmt.Sprintf("%s-%d.%s", ext, time.Now().UnixNano(), ext))
	defer os.Remove(tmpPath)

	cliArgs := strings.Fields(cliCmd)
	cliArgs = append(cliArgs, tmpPath)
	if _, err := b.execCLI(ctx, nil, cliArgs...); err != nil {
		return nil, &proto.ExecError{Action: "browser", Reason: fmt.Sprintf("%s: %s", cliCmd, err)}
	}
	data, err := os.ReadFile(tmpPath)
	if err != nil {
		return nil, &proto.ExecError{Action: "browser", Reason: fmt.Sprintf("%s: %s", cliCmd, err)}
	}
	if err := env.VFS.MkdirAll(dirOfVFS(abs), 0o755); err != nil {
		return nil, err
	}
	if err := env.VFS.WriteFile(abs, data, 0o644); err != nil {
		return nil, &proto.ExecError{Action: "browser", Reason: fmt.Sprintf("%s: %s", cliCmd, err)}
	}
	b.markDirty()
	r.Attrs["path"] = abs
	r.Content = fmt.Sprintf(msg, abs)
	return r, nil
}

// postProcess 处理 snapshot/tab 的 attrs（§5.6.3）。
func (b *Browser) postProcess(sub, out string, r *vcore.Result, err error) (string, error) {
	if err != nil {
		return out, err
	}
	switch sub {
	case "snapshot":
		r.Attrs["rows"] = strconv.Itoa(countSnapshotRows(out))
	case "tab":
		r.Attrs["rows"] = strconv.Itoa(countNonEmptyLines(out))
	}
	return out, nil
}

// sleepAction 实现 sleep（本地计时，不过 CLI）。
func sleepAction(ctx context.Context, args []string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("duration is required")
	}
	d, err := time.ParseDuration(args[0])
	if err != nil {
		return "", fmt.Errorf("invalid duration %q", args[0])
	}
	if d <= 0 {
		return "", fmt.Errorf("duration must be positive, got %q", args[0])
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(d):
	}
	return fmt.Sprintf("✓ slept for %s", args[0]), nil
}

func countSnapshotRows(s string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " "), "- ") {
			n++
		}
	}
	return n
}

func countNonEmptyLines(s string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

var _ = path.Base
