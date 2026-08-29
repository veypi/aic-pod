package host

import (
	"context"
	"os"
	"path/filepath"

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
	// sid 绑定策略视图：文件交换（upload 读 / download 等写）经 CheckPolicy 判定，
	// deny 名单与会话 grant/会话区在此生效（v0.14.5 评审二轮：修复前四通道漏检）
	res, err := b.Handle(ctx, c.newEnv(sid, ""), req.MsgID, argv)
	return resultToResponse(req.MsgID, res, err)
}

// sessionWorkDir 返回会话工作区（v0.14.5 §4 布局，两端同构 UserOutputDir/sessions/{sid}）：
// $HOME/.aic/sessions/{sid}——exec 日志（.exec/）、browser 交换（.browser/）、
// 截图（.screenshot/）的落点。PublicDir 不可得时回落系统临时目录旧位
// （{tmp}/aic/{sid}，临时产物语义不变）。
func sessionWorkDir(sid string) string {
	if dir, err := cfg.PublicDir(); err == nil {
		return filepath.Join(dir, "sessions", sid)
	}
	return filepath.Join(os.TempDir(), "aic", sid)
}

// browserFor 返回 per-session browser 实例（懒建）：
// Browser 的 curID/lastResult 为实例状态（§5.6 stateful 串行），
// 不同 session 并发调用需独立实例。
// 布局（v0.14.5 §4，与 cloud 同构）：
//   - 交换目录（upload 暂存 / download 与截图中转）：$HOME/.aic/sessions/{sid}/.browser
//   - 截图产物：$HOME/.aic/sessions/{sid}/.screenshot
//   - CLI 输出落盘：$HOME/.aic/sessions/{sid}/.exec/{msg_id}.log
//   - state（cookies/storage）：$HOME/.aic/.cache/browser/browser.json（用户级共享，
//     按站点 merge——不同 session 访问不同站点不互覆，实例创建自动 load）
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
	workDir := sessionWorkDir(sid)
	tempDir := filepath.Join(workDir, ".browser")
	// 交换目录预建（CLI 直接读写；0700 仅本用户）
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		logv.Warn().Msgf("browser: create state dir: %v", err)
	}
	statePath := ""
	if dir, err := cfg.PublicDir(); err == nil {
		statePath = filepath.Join(dir, ".cache", "browser", "browser.json")
	}
	b := vbrowser.New(vbrowser.Config{
		TempDir:       tempDir,
		StatePath:     statePath, // 空 = 不自动保存（PublicDir 不可得的罕见场景）
		ScreenshotDir: filepath.Join(workDir, ".screenshot"),
		// §5.10/§5.6：pod 模式不隔离，免沙箱（沙箱下 Chrome 冷启动必挂）
		NoSandbox: true,
		// §5.9：CLI 子进程经 exec_procs 统一托管，输出落盘 {会话工作区}/.exec/{msg_id}.log
		ExecProcs: c.procs,
		LogPathFn: func(msgID string) string {
			return filepath.Join(workDir, ".exec", msgID+".log")
		},
	})
	c.browsers[sid] = b
	return b
}
