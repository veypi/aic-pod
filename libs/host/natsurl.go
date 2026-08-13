package host

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/veypi/aic-pod/cfg"
)

// natsPath 是服务端 NATS WebSocket 挂载路径（Router.Extend("/api/nc")）。
const natsPath = "/api/nc"

// ResolveNATSURL 由平台地址推导 NATS WebSocket 端点（唯一来源，无显式 url 配置）：
//
//   - 协议按 scheme 推断：https→wss、http→ws、ws/wss 原样保留；无 scheme 按 https 补全
//   - host 可携带路径前缀（产品壳挂载场景）：前缀保留并拼接 /api/nc
//
// 例：
//
//	https://ivec.ai                  → wss://ivec.ai/api/nc
//	http://localhost:4000            → ws://localhost:4000/api/nc
//	http://127.0.0.1:4000/rses/aiv   → ws://127.0.0.1:4000/rses/aiv/api/nc
func ResolveNATSURL(hostURL string) string {
	h := strings.TrimSpace(hostURL)
	if h == "" {
		h = cfg.DefaultHost
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
	prefix := strings.TrimSuffix(u.Path, "/")
	return fmt.Sprintf("%s://%s%s%s", scheme, u.Host, prefix, natsPath)
}
