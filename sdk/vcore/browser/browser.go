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
	"strings"
	"sync"
	"time"

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
}

// Browser 是 browser 虚拟指令实例：每个 (session, host) 一个。
type Browser struct {
	cfg       Config
	mu        sync.Mutex
	saveTimer *time.Timer
	dirty     bool
}

// VirtualDecl 返回 caps 声明元数据（§6.3）：stateful 串行 + download/wait 可后台化。
func VirtualDecl() proto.VirtualDecl {
	return proto.VirtualDecl{Name: "browser", RequiredLevel: proto.LevelWrite, Stateful: true, Backgroundable: true}
}

// Subcommands 是核心 13 + upload（§5.6；var/pipeline/save 不迁移——
// var→mem、pipeline→Agent 多次调用、save→自动保存）。
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

// Close 冲刷 state 自动保存并清理临时目录。
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
	return os.RemoveAll(b.cfg.TempDir)
}

// Handle 是 vcore 虚拟指令入口：exec browser <subcommand> [args...]。
func (b *Browser) Handle(ctx context.Context, env *vcore.Env, argv []string) (*vcore.Result, error) {
	if len(argv) == 0 {
		return nil, &proto.ExecError{Action: "browser",
			Reason: "subcommand is required (supported: " + strings.Join(Subcommands, ", ") + ")"}
	}
	sub := argv[0]
	args := argv[1:]
	for _, s := range Subcommands {
		if s == sub {
			return b.run(ctx, env, sub, args)
		}
	}
	return nil, &proto.ExecError{Action: "browser",
		Reason: fmt.Sprintf("unknown subcommand %q (supported: %s)", sub, strings.Join(Subcommands, ", "))}
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

// execCLI 调用 agent-browser CLI，返回标准输出（错误时返回 trimmed 输出作为错误）。
func (b *Browser) execCLI(ctx context.Context, globalFlags []string, args ...string) (string, error) {
	cmdArgs := []string{
		"--namespace", b.cfg.Namespace,
		"--session", b.cfg.Session,
		"--user-agent", b.cfg.UserAgent,
	}
	cmdArgs = append(cmdArgs, globalFlags...)
	cmdArgs = append(cmdArgs, args...)
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
