package execution

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	pathpkg "path"
	"strings"
)

// DefaultPlatformAuthPrefixes 是 exec curl 的默认平台身份注入面。
// 它只控制身份注入，不限制其他 /xxx 平台路径或公网 URL 的访问。
var DefaultPlatformAuthPrefixes = []string{"/skills/cloud/"}

// PlatformHTTPConfig 配置 cloud/host 共用的平台相对 URL 支持。
// BaseURL 只用于补全单斜杠开头的路径；Handler 非空时，同 origin 请求直接进程内派发。
// Inject 只在同 origin 且路径命中 AuthPrefixes 时调用，可注入 Bearer 或受信上下文身份。
type PlatformHTTPConfig struct {
	BaseURL      string
	AuthPrefixes []string
	Inject       func(ctx context.Context, req *http.Request) error
	Handler      http.Handler
}

func (c *NetClient) resolvePlatformURL(raw string) (string, error) {
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return raw, nil
	}
	if c.cfg.Platform == nil || strings.TrimSpace(c.cfg.Platform.BaseURL) == "" {
		return "", fmt.Errorf("relative platform URL %q requires a configured platform base URL", raw)
	}
	base, err := url.Parse(c.cfg.Platform.BaseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return "", fmt.Errorf("invalid platform base URL %q", c.cfg.Platform.BaseURL)
	}
	ref, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid platform path %q: %w", raw, err)
	}
	return base.ResolveReference(ref).String(), nil
}

func (c *NetClient) isPlatformURL(raw string) bool {
	if c.cfg.Platform == nil {
		return false
	}
	return sameOrigin(c.cfg.Platform.BaseURL, raw)
}

func (c *NetClient) isInternalPlatformURL(raw string) bool {
	return c.cfg.Platform != nil && c.cfg.Platform.Handler != nil && c.isPlatformURL(raw)
}

func (c *NetClient) shouldInjectPlatformAuth(raw string) bool {
	if c.cfg.Platform == nil || !c.isPlatformURL(raw) {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	cleaned := pathpkg.Clean("/" + strings.TrimPrefix(u.Path, "/"))
	for _, prefix := range c.cfg.Platform.AuthPrefixes {
		if prefix != "" && strings.HasPrefix(cleaned, prefix) {
			return true
		}
	}
	return false
}

func sameOrigin(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	if errA != nil || errB != nil || ua.Scheme == "" || ua.Host == "" || ub.Scheme == "" || ub.Host == "" {
		return false
	}
	return strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host)
}
