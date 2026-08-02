package host

import (
	"context"
	"os"
	"path/filepath"

	"github.com/veypi/aic-pod/sdk/proto"
	vbrowser "github.com/veypi/aic-pod/sdk/vcore/browser"
)

// runBrowser 执行 browser 虚拟指令（§5.6 pod 模式）：agent-browser CLI，
// **不隔离**——不传 --session/--namespace（用户本机默认浏览器环境，边界即用户自身）；
// 文件交换走 OS VFS（路径不限制）；CLI 子进程经 exec_procs 统一托管（§5.9）。
func (c *Client) runBrowser(ctx context.Context, sid string, req *proto.ToolRequest, argv []string) *proto.ToolResponse {
	b := c.browserFor(sid)
	res, err := b.Handle(ctx, c.newEnv(""), req.MsgID, argv)
	return resultToResponse(req.MsgID, res, err)
}

// browserFor 返回 per-session browser 实例（懒建）：
// Browser 的 curID/lastResult 为实例状态（§5.6 stateful 串行），
// 不同 session 并发调用需独立实例。
func (c *Client) browserFor(sid string) *vbrowser.Browser {
	c.browserMu.Lock()
	defer c.browserMu.Unlock()
	if b, ok := c.browsers[sid]; ok {
		return b
	}
	b := vbrowser.New(vbrowser.Config{
		TempDir: filepath.Join(os.TempDir(), "aic-browser-"+sid),
		// §5.9：CLI 子进程经 exec_procs 统一托管，输出落盘 {tmp}/aic/{sid}/{msg_id}.log
		ExecProcs: c.procs,
		LogPathFn: func(msgID string) string {
			return filepath.Join(os.TempDir(), "aic", sid, msgID+".log")
		},
	})
	c.browsers[sid] = b
	return b
}
