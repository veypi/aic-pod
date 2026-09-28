package host

import (
	"crypto/sha256"
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
// 其余按工具请求处理（hosts_nats/2）：验签 → deadline 过期拒绝 → nonce
// 窗口去重 → 分发（不再重新分类审批，grant_approved 随签名信封入可信上下文）。
func (c *Client) handleMsg(msg *nats.Msg) {
	if strings.HasSuffix(msg.Subject, ".tools.req") {
		c.handleToolsMsg(msg)
		return
	}
	if strings.HasSuffix(msg.Subject, ".rtc.in") {
		c.handleRTCSignal(msg.Data)
	}
}

func (c *Client) sessionWorkDir(sid string) string {
	if c.sessionRoot != "" {
		return filepath.Join(c.sessionRoot, sid)
	}
	return filepath.Join(os.TempDir(), "aic", sid)
}

// execLogPaths 返回会话执行日志路径对 .exec/{short}.stdout.log / .stderr.log
// （hosts-vsh-redesign §3.2：stdout/stderr 分别全量持久化）。短名 =
// sha256(sid + "\x00" + id) 前 6 字节（12 hex）——同会话内唯一、可复现。
func (c *Client) execLogPaths(sid, id string) (string, string) {
	sum := sha256.Sum256([]byte(sid + "\x00" + id))
	base := filepath.Join(c.sessionWorkDir(sid), ".exec", fmt.Sprintf("%x", sum[:6]))
	return base + ".stdout.log", base + ".stderr.log"
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
