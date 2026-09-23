package host

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/veypi/aic-pod/libs/proto"
)

// handleMsg 处理一条入站消息：rtc.in 信令路由到 RTC 服务（不参与验签流程——
// 信令身份由 NATS 权限模型保证，DataChannel 使用绑定 DTLS 证书的短期票据）；
// 其余按工具请求处理（§6.2 host 端验证规范）：
// 验签 → deadline 过期拒绝 → nonce 窗口去重 → granted_level 纵深检查 → 分发。
func (c *Client) handleMsg(msg *nats.Msg) {
	if strings.HasSuffix(msg.Subject, ".tools.req") {
		c.handleToolsMsg(msg)
		return
	}
	if strings.HasSuffix(msg.Subject, ".rtc.in") {
		c.handleRTCSignal(msg.Data)
	}
}

// execCmd 是 exec 工具请求的 pod 入口（vsh 引擎化，todo 3.1.3）：唯一契约 =
// script（{script, workdir?, timeout?, stdin?, nosandbox?}），pod 侧引擎执行。
// 旧 action/argv 通道（curl/git/json/bg_*/本地命令逐名注册）已随 vcore 删除
// 退役——命令发现经脚本内 `commands`，后台经脚本内 `bg`，授权经脚本内 `grant`。
func (c *Client) execCmd(ctx context.Context, sid string, req *proto.ToolRequest) *proto.ToolResponse {
	var p execScriptParams
	if err := json.Unmarshal(req.Data, &p); err != nil {
		return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateError, Error: "invalid exec data: " + err.Error()}
	}
	if strings.TrimSpace(p.Script) == "" {
		return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateError,
			Error: "exec: script is required（action/argv 通道已下线——命令发现用脚本内 `commands`，授权用 `grant`，后台用 `bg`）"}
	}
	return c.execScript(ctx, sid, req, p)
}

func (c *Client) sessionWorkDir(sid string) string {
	if c.sessionRoot != "" {
		return filepath.Join(c.sessionRoot, sid)
	}
	return filepath.Join(os.TempDir(), "aic", sid)
}

// execLogPath 返回会话执行日志路径 .exec/{short}.log（2026-09-23 用户定稿：
// 原 exec_{epoch}/{id}.log 名过长；短名 = sha256(epoch + "\x00" + id) 前 6 字节
// （12 hex）——同 epoch 内唯一、可复现，且不再随请求 id 长度膨胀）。
func (c *Client) execLogPath(sid, id string) string {
	sum := sha256.Sum256([]byte(c.procs.Epoch() + "\x00" + id))
	return filepath.Join(c.sessionWorkDir(sid), ".exec", fmt.Sprintf("%x.log", sum[:6]))
}

// ensureSessionWorkDir 确保会话工作区（含父级）就绪。嵌套执行路径
// （exec_procs.Output(ctx) 非空，平台命令的常态）不经 StartCall 的
// LogPath 建目录动作，必须显式确保：windows 沙箱的可写根授予要求目录
// 已存在（grant 时枚举），缺目录会让首个嵌套命令沙箱初始化失败。
func (c *Client) ensureSessionWorkDir(sid string) error {
	if err := os.MkdirAll(c.sessionWorkDir(sid), 0o700); err != nil {
		return fmt.Errorf("prepare session workdir: %s", err)
	}
	return nil
}

func reject(msgID, reason string) *proto.ToolResponse {
	return &proto.ToolResponse{MsgID: msgID, State: proto.StateRejected, Error: reason}
}

func deviceInfo() *proto.DeviceInfo {
	return &proto.DeviceInfo{OS: runtime.GOOS, Arch: runtime.GOARCH, NumCPU: runtime.NumCPU()}
}

func mustNonce() string {
	n, err := proto.NewNonce()
	if err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return n
}

type wsURL struct {
	base      string
	proxyPath string
}

func parseWSURL(raw string) (*wsURL, error) {
	// ws(s)://host/path → base = ws(s)://host，path 作为 ProxyPath。
	// 扫描起点须按实际 scheme（ws:// 与 wss:// 长度不同）跳过 "://"，
	// 否则 wss:// 的第二个斜杠会被误判为路径起点。
	i := strings.Index(raw, "://")
	if i < 0 {
		return &wsURL{base: raw}, nil
	}
	for j := i + 3; j < len(raw); j++ {
		if raw[j] == '/' {
			if j == len(raw)-1 {
				return &wsURL{base: raw}, nil
			}
			return &wsURL{base: raw[:j], proxyPath: raw[j:]}, nil
		}
	}
	return &wsURL{base: raw}, nil
}
