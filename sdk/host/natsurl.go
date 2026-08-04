package host

import (
	"fmt"
	"net/url"
	"strings"
)

// DefaultHost 是 CLI 默认平台地址。
const DefaultHost = "https://ivec.ai"

// natsPath 是服务端 NATS WebSocket 挂载路径（aic Router.Extend("/nc") 挂载在 /aic 前缀下）。
const natsPath = "/aic/api/nc"

// ResolveNATSURL 将 host/-url 参数归一为 NATS WebSocket 端点：
//
//   - url 非空：原样返回（显式直连 ws://wss://nats:// 均可）
//   - url 为空：按 host 推断——https→wss、http→ws，取 origin（忽略路径）拼接
//     /aic/api/nc；host 无 scheme 时按 https 补全；ws/wss scheme 原样保留
//
// 例：https://ivec.ai → wss://ivec.ai/aic/api/nc
//
//	http://localhost:4000 → ws://localhost:4000/aic/api/nc
func ResolveNATSURL(hostURL, natsURL string) string {
	if s := strings.TrimSpace(natsURL); s != "" {
		return s
	}
	h := strings.TrimSpace(hostURL)
	if h == "" {
		h = DefaultHost
	}
	if !strings.Contains(h, "://") {
		h = "https://" + h
	}
	u, err := url.Parse(h)
	if err != nil || u.Host == "" {
		// 解析失败时按 https 语义兜底拼接
		return "wss://" + strings.TrimPrefix(h, "https://") + natsPath
	}
	scheme := u.Scheme
	switch scheme {
	case "https":
		scheme = "wss"
	case "http":
		scheme = "ws"
	}
	return fmt.Sprintf("%s://%s%s", scheme, u.Host, natsPath)
}
