package browser

import (
	"context"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/veypi/aic-pod/sdk/proto"
	"github.com/veypi/aic-pod/sdk/vcore"
)

// run 分发子命令（§5.6.3 输入输出标准，agent-browser 基准）。
func (b *Browser) run(ctx context.Context, env *vcore.Env, sub string, args []string) (*vcore.Result, error) {
	r := &vcore.Result{Attrs: map[string]string{"action": "browser"}}
	var out string
	var err error

	switch sub {
	case "open":
		out, err = b.open(ctx, args)
	case "click", "eval", "get", "snapshot", "tab", "wait":
		out, err = b.execCLI(ctx, nil, append([]string{sub}, args...)...)
		out, err = b.postProcess(sub, out, r, err)
	case "close":
		out, err = b.execCLI(ctx, nil, "close")
		if err == nil {
			// 统一语义：关闭当前页面/会话；cloud 关闭最后页面时会话随之销毁（§5.6.3）
			r.Attrs["closed"] = "true"
			b.markDirty()
		}
	case "read":
		out, err = b.execCLI(ctx, nil, append([]string{"read"}, args...)...)
		if err == nil {
			// read 上限 100K 字节，超出尾部追加 "\n... (truncated)"（§5.6.3）
			if len(out) > 100*1024 {
				out = strings.TrimSuffix(out[:100*1024], "...") + "\n... (truncated)"
				r.Attrs["truncated"] = "true"
			} else {
				r.Attrs["truncated"] = "false"
			}
		}
	case "network":
		out, err = b.network(ctx, args, r)
	case "download":
		return b.download(ctx, env, args)
	case "screenshot":
		return b.screenshot(ctx, env, args)
	case "sleep":
		out, err = sleepAction(ctx, args)
	case "upload":
		return b.upload(ctx, env, args)
	default:
		return nil, &proto.ExecError{Action: "browser", Reason: "unknown subcommand " + sub}
	}

	if err != nil {
		return nil, &proto.ExecError{Action: "browser", Reason: fmt.Sprintf("%s: %s", sub, err)}
	}
	if out == "" {
		out = "(no output)"
	}
	r.Content = out
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

// network 处理请求检查：详情模式（[id]）与列表模式（环形缓冲 500 条，--limit 默认 100）。
func (b *Browser) network(ctx context.Context, args []string, r *vcore.Result) (string, error) {
	detail := len(args) > 0 && !strings.HasPrefix(args[0], "--")
	sub := "requests"
	if detail {
		sub = "request"
	}
	out, err := b.execCLI(ctx, nil, append([]string{"network", sub}, args...)...)
	if err != nil || detail {
		return out, err
	}
	// 列表模式：按 --limit（默认 100）截断并标 truncated（§5.6.3）
	limit := 100
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--limit" {
			if n, e := strconv.Atoi(args[i+1]); e == nil && n > 0 {
				limit = n
			}
		}
	}
	lines := strings.Split(out, "\n")
	rows := len(lines)
	if rows > limit {
		lines = lines[:limit]
		out = strings.Join(lines, "\n")
	}
	r.Attrs["rows"] = strconv.Itoa(min(rows, limit))
	r.Attrs["truncated"] = strconv.FormatBool(rows > limit)
	return out, nil
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
