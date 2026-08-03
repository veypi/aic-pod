// Package browser 实现 exec browser 虚拟指令（docs/instruction_sets_v2.md §5.6）：
// agent-browser 指令映射（核心 13 + upload），以 agent-browser CLI 为唯一基准。
//
// 差异只在隔离程度，由引入方配置（§5.6）：
//   - cloud（aic 引入）：严格隔离 + state 自动保存——独立实例/profile/会话数据目录；
//   - pod 引入：不强制隔离——用户本机浏览器环境。
//
// 文件交换走 VFS 接口（不新增适配点）：upload 源文件经 VFS 读，
// download 落盘与 screenshot 经 VFS 写（cloud = $SESSION 根约束策略）。
package browser

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/veypi/aic-pod/sdk/exec_procs"
	"github.com/veypi/aic-pod/sdk/proto"
	"github.com/veypi/aic-pod/sdk/vcore"
)

// Config 是 browser 实例配置（隔离策略注入接口，附录 C）。
type Config struct {
	// ExecPath 是 agent-browser 可执行文件路径，缺省 "agent-browser"（PATH 查找）。
	ExecPath string
	// Namespace 是浏览器实例命名空间（cloud = userID 严格隔离；pod = 本机默认值）。
	Namespace string
	// Session 是浏览器会话 id（同 (session, host) 串行由调用方保证，§5.6 stateful）。
	Session string
	// UserAgent 缺省桌面 Chrome UA。
	UserAgent string
	// StatePath 是浏览器 state（cookies/storage）自动落盘的 OS 路径（平台行为，
	// 不占指令位，§5.6）；空 = 不自动保存。cloud = 会话数据目录；pod 按需。
	StatePath string
	// TempDir 是文件交换临时目录（upload 暂存 / download 与 screenshot 落地），
	// 缺省 os.TempDir()/aic-browser-{session}。
	TempDir string
	// ExecProcs 是 exec 子进程统一托管（§5.9）：非 nil 时每次 CLI 调用经其管理——
	// 输出重定向 LogPathFn 指定路径、请求超时自动后台化、前 MaxLines 行返回。
	// nil = 直连模式（测试/无托管）。
	ExecProcs *exec_procs.Manager
	// LogPathFn 生成每次 CLI 调用的输出落盘路径（ExecProcs 非 nil 时必填）。
	LogPathFn func(msgID string) string
}

// Browser 是 browser 虚拟指令实例：每个 (session, host) 一个。
type Browser struct {
	cfg       Config
	mu        sync.Mutex
	saveTimer *time.Timer
	dirty     bool

	// 当前调用上下文（§5.6 stateful：同 (session, host) 串行，单调用安全）：
	curID      string            // 本次调用的 msgID（后台条目 ID / 落盘文件名）
	lastResult *exec_procs.Result // 最近一次 CLI 调用的托管结果（path/background 等）
}

// Subcommands 是 vcore 平台特化子命令集（§5.6：安全/文件交换/截断/本地实现）。
// 其余 agent-browser 子命令一律透传（以 agent-browser CLI 为唯一基准，vcore 不做白名单）。
var Subcommands = []string{
	"open", "click", "close", "download", "eval", "get", "network",
	"read", "screenshot", "snapshot", "tab", "wait", "sleep", "upload",
}

const defaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// New 创建 browser 实例。
func New(cfg Config) *Browser {
	if cfg.ExecPath == "" {
		cfg.ExecPath = "agent-browser"
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = defaultUserAgent
	}
	if cfg.TempDir == "" {
		cfg.TempDir = filepath.Join(os.TempDir(), "aic-browser-"+cfg.Session)
	}
	return &Browser{cfg: cfg}
}

// Close 冲刷 state 自动保存、关闭隔离会话（cloud）并清理临时目录。
// cloud（Namespace 非空）下同步关闭 agent-browser 会话：aic runtime 结束/
// 休眠时若不清 daemon 会话，Chrome for Testing 实例与 active session 会一直
// 驻留（session list 越积越多）。pod 模式（Namespace 空）不关——用户本机
// 浏览器环境不属于平台托管。
func (b *Browser) Close() error {
	b.mu.Lock()
	if b.saveTimer != nil {
		b.saveTimer.Stop()
		b.saveTimer = nil
	}
	dirty := b.dirty
	b.dirty = false
	b.mu.Unlock()
	if dirty {
		b.saveState(context.Background())
	}
	if b.cfg.Namespace != "" {
		b.closeSession(context.Background())
	}
	return os.RemoveAll(b.cfg.TempDir)
}

// closeSession 关闭当前隔离会话（agent-browser close，--namespace/--session 由
// execCLI 同款参数规则拼接）。直连执行不经 ExecProcs 托管：Close 时 curID/LogPath
// 可能已过期，且该调用无需输出回传。会话不存在时 close 无副作用（幂等）。
func (b *Browser) closeSession(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmdArgs := []string{}
	if b.cfg.Namespace != "" {
		cmdArgs = append(cmdArgs, "--namespace", b.cfg.Namespace)
	}
	if b.cfg.Session != "" {
		cmdArgs = append(cmdArgs, "--session", b.cfg.Session)
	}
	cmdArgs = append(cmdArgs, "close")
	cmd := exec.CommandContext(ctx, b.cfg.ExecPath, cmdArgs...)
	_ = cmd.Run() // 失败（daemon 不在/会话已关）忽略，Close 不因清理失败报错
}

// Handle 是 vcore 虚拟指令入口：exec browser <subcommand> [args...]。
// Handle 执行一次 browser 指令（§5.6）：msgID 为调用方消息 ID（后台条目 ID / 落盘文件名）。
// 同一 Browser 实例串行调用（§5.6 stateful），msgID 仅用于当前调用上下文。
func (b *Browser) Handle(ctx context.Context, env *vcore.Env, msgID string, argv []string) (*vcore.Result, error) {
	if len(argv) == 0 {
		return nil, &proto.ExecError{Action: "browser",
			Reason: "subcommand is required (see `browser -h`)"}
	}
	b.curID = msgID
	b.lastResult = nil
	return b.run(ctx, env, argv[0], argv[1:])
}

// markDirty 在成功动作后调度 state 自动保存（防抖 1s；close 时同步冲刷）。
func (b *Browser) markDirty() {
	if b.cfg.StatePath == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.dirty = true
	if b.saveTimer != nil {
		b.saveTimer.Stop()
	}
	b.saveTimer = time.AfterFunc(time.Second, func() {
		b.mu.Lock()
		b.dirty = false
		b.mu.Unlock()
		b.saveState(context.Background())
	})
}

// saveState 执行 state 自动落盘（cookies/storage → 会话数据目录，§5.6）。
func (b *Browser) saveState(ctx context.Context) {
	if b.cfg.StatePath == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(b.cfg.StatePath), 0o700)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, _ = b.execCLI(ctx, nil, "state", "save", b.cfg.StatePath)
}

// execCLI 调用 agent-browser CLI（§5.9）：
//   - ExecProcs 托管模式：输出重定向 LogPathFn(msgID)，请求超时自动后台化（
//     转后台时 lastResult.Background=true + id），返回前 MaxLines 行
//   - 直连模式（无托管）：CombinedOutput 内存合并
func (b *Browser) execCLI(ctx context.Context, globalFlags []string, args ...string) (string, error) {
	// 空值不传参：cloud 传 --namespace/--session（隔离）；pod 不传（用户本机默认环境，§5.6）
	cmdArgs := []string{}
	if b.cfg.Namespace != "" {
		cmdArgs = append(cmdArgs, "--namespace", b.cfg.Namespace)
	}
	if b.cfg.Session != "" {
		cmdArgs = append(cmdArgs, "--session", b.cfg.Session)
	}
	if b.cfg.UserAgent != "" {
		cmdArgs = append(cmdArgs, "--user-agent", b.cfg.UserAgent)
	}
	cmdArgs = append(cmdArgs, globalFlags...)
	cmdArgs = append(cmdArgs, args...)

	if b.cfg.ExecProcs != nil && b.cfg.LogPathFn != nil {
		full := append([]string{b.cfg.ExecPath}, cmdArgs...)
		res, err := b.cfg.ExecProcs.Start(ctx, exec_procs.StartOptions{
			ID:      b.curID,
			Command: strings.Join(full, " "),
			LogPath: b.cfg.LogPathFn(b.curID),
			Exec:    full,
		})
		if err != nil {
			return "", err
		}
		b.lastResult = res
		return res.Content, nil
	}

	cmd := exec.CommandContext(ctx, b.cfg.ExecPath, cmdArgs...)
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return "", ctx.Err()
	}
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s", msg)
	}
	return strings.TrimSpace(string(out)), nil
}

// applyExecAttrs 在 run 出口补托管结果 attrs（§5.9）：path/rows/truncated/background/id。
func (b *Browser) applyExecAttrs(r *vcore.Result) {
	res := b.lastResult
	if res == nil {
		return
	}
	r.Attrs["path"] = res.LogPath
	r.Attrs["rows"] = strconv.Itoa(res.Lines)
	if res.Truncated {
		r.Attrs["truncated"] = "true"
	}
	if res.Background {
		r.Attrs["background"] = "true"
		r.Attrs["id"] = res.ID
	}
}
