package host

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/proto"
	vbrowser "github.com/veypi/aic-pod/libs/vcore/browser"
	"github.com/veypi/vigo/logv"
)

// runBrowser 执行 browser 虚拟指令（§5.6 pod 模式）：agent-browser CLI，
// **不隔离**——不传 --session/--namespace（用户本机默认浏览器环境，边界即用户自身）；
// 文件交换走 OS VFS（路径不限制）；CLI 子进程经 exec_procs 统一托管（§5.9）。
func (c *Client) runBrowser(ctx context.Context, sid string, req *proto.ToolRequest, argv []string) *proto.ToolResponse {
	b := c.browserFor(sid)
	res, err := b.Handle(ctx, c.newEnv(""), req.MsgID, argv)
	return resultToResponse(req.MsgID, res, err)
}

// browserStateDir 返回 browser 工具状态根目录：优先 UserConfigDir/aic（持久）。
// 配置目录不可用（罕见）时回落进程级唯一临时目录（MkdirTemp 0700，路径不可
// 预测——不用共享 /tmp 下的固定路径，防多用户机器上被同名预创建占位），进程
// 生命周期内只建一次。
var browserStateDir = sync.OnceValue(func() string {
	if dir, err := cfg.StateDir(); err == nil {
		return dir
	} else {
		logv.Warn().Msgf("browser: state dir unavailable: %v (fallback to unique temp dir)", err)
	}
	if d, err := os.MkdirTemp("", "aic-browser-state-"); err == nil {
		return d
	}
	return os.TempDir()
})

// browserFor 返回 per-session browser 实例（懒建）：
// Browser 的 curID/lastResult 为实例状态（§5.6 stateful 串行），
// 不同 session 并发调用需独立实例。
// 工具自身状态（交换中转/临时产物）落配置目录/临时目录，不碰用户工作区：
//   - TempDir（upload 暂存 / download 与截图中转）：UserConfigDir/aic/browser/{sid}
//   - 截图最终产物：{tmp}/aic/screenshot（临时使用，删除无碍）
//
// 沙箱（§5.10）：browser 显式免沙箱（NoSandbox）——pod 模式语义即不隔离
// （§5.6），且沙箱下 Chrome 冷启动必挂（实测）；闸门在服务端审批（browser
// 声明 level 2 + host checkGranted 纵深），文件效应全部由 host 进程经 VFS 完成。
func (c *Client) browserFor(sid string) *vbrowser.Browser {
	c.browserMu.Lock()
	defer c.browserMu.Unlock()
	if b, ok := c.browsers[sid]; ok {
		return b
	}
	tempDir := filepath.Join(browserStateDir(), "browser", sid)
	// 交换目录预建（CLI 直接读写；0700 仅本用户）
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		logv.Warn().Msgf("browser: create state dir: %v", err)
	}
	b := vbrowser.New(vbrowser.Config{
		TempDir: tempDir,
		// 截图产物：系统临时目录（临时使用，不污染用户工作区/配置目录）
		ScreenshotDir: filepath.ToSlash(filepath.Join(os.TempDir(), "aic", "screenshot")),
		// §5.10/§5.6：pod 模式不隔离，免沙箱（沙箱下 Chrome 冷启动必挂）
		NoSandbox: true,
		// §5.9：CLI 子进程经 exec_procs 统一托管，输出落盘 {tmp}/aic/{sid}/{msg_id}.log
		ExecProcs: c.procs,
		LogPathFn: func(msgID string) string {
			return filepath.Join(os.TempDir(), "aic", sid, msgID+".log")
		},
	})
	c.browsers[sid] = b
	return b
}
