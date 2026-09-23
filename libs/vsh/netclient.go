package vsh

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/veypi/vbox"
	vshnet "github.com/veypi/vsh/network"
)

// NetClient 默认值（design §4.3：重定向上限/超时/响应上限）。
const (
	DefaultNetMaxRedirects     = 10
	DefaultNetTimeout          = 10 * time.Minute
	DefaultNetMaxResponseBytes = MaxFileBytes // 64 MiB
)

// NetAudit 网络审计记录（URL/状态/大小/耗时——验收 5）。
type NetAudit struct {
	Method   string
	URL      string // 最终请求 URL（重定向后）
	Status   int    // HTTP 状态码；未发出 = 0
	Bytes    int64  // 响应体字节数
	Duration time.Duration
	Blocked  bool   // 被策略/私网阻断
	Reason   string // 阻断/错误原因
}

// NetClientConfig cloud NetClient 装配（唯一网络权威；网络不触发审批）。
type NetClientConfig struct {
	// Rules NetRuleSet 快照源（default open + grant net 动态行）；nil = 全开放。
	Rules func() vbox.NetRuleSet
	// Audit 每次请求结束（含阻断）回调；nil = 不审计。
	Audit func(NetAudit)
	// OnResponseSize 下载配额预检（cloudenv.go:116 Fetcher 迁移）：响应
	// Content-Length > 0 时读体前调用；返回非 nil 即断流。nil = 不预检
	//（写盘路径仍由 FS backing 的 QuotaFS 逐块闸门兜底——curl -o 不绕配额）。
	OnResponseSize func(size int64) error
	MaxRedirects   int           // ≤0 = DefaultNetMaxRedirects
	Timeout        time.Duration // ≤0 = DefaultNetTimeout（req.Timeout 优先）
	MaxResponseBytes int64       // ≤0 = DefaultNetMaxResponseBytes
	// DialContext/Resolver 测试注入；nil = 系统默认。
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	Resolver    interface {
		LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
	}
}

// NetClient 实现 vsh network.Client：NetRuleSet 门 + 私网阻断 + 重定向逐跳
// 复核 + 响应上限 + 审计。
type NetClient struct {
	cfg NetClientConfig
}

func NewNetClient(cfg NetClientConfig) *NetClient {
	if cfg.MaxRedirects <= 0 {
		cfg.MaxRedirects = DefaultNetMaxRedirects
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultNetTimeout
	}
	if cfg.MaxResponseBytes <= 0 {
		cfg.MaxResponseBytes = DefaultNetMaxResponseBytes
	}
	return &NetClient{cfg: cfg}
}

// Do 执行一次 HTTP 请求（含手动重定向循环——每跳重新过门，防重定向逃逸）。
func (c *NetClient) Do(ctx context.Context, req *vshnet.Request) (*vshnet.Response, error) {
	start := time.Now()
	audit := NetAudit{Method: req.Method, URL: req.URL}
	defer func() { audit.Duration = time.Since(start) }()

	current := req.URL
	body := req.Body
	for hop := 0; ; hop++ {
		if err := c.checkTarget(ctx, current); err != nil {
			audit.Blocked = true
			audit.Reason = err.Error()
			c.emit(audit)
			return nil, err
		}
		resp, err := c.round(ctx, req, current, body)
		if err != nil {
			audit.Reason = err.Error()
			c.emit(audit)
			return nil, err
		}
		audit.Status = resp.StatusCode
		audit.URL = current
		if !isRedirectStatus(resp.StatusCode) || !req.FollowRedirects {
			data, rerr := c.readBody(resp, current)
			_ = resp.Body.Close()
			if rerr != nil {
				audit.Reason = rerr.Error()
				c.emit(audit)
				return nil, rerr
			}
			audit.Bytes = int64(len(data))
			c.emit(audit)
			return &vshnet.Response{
				StatusCode: resp.StatusCode,
				Status:     resp.Status,
				Headers:    flattenHeaders(resp.Header),
				Body:       data,
				URL:        current,
			}, nil
		}
		loc := resp.Header.Get("Location")
		_ = resp.Body.Close()
		if loc == "" || hop >= c.cfg.MaxRedirects {
			audit.Reason = "too many redirects"
			c.emit(audit)
			return nil, &vshnet.TooManyRedirectsError{MaxRedirects: c.cfg.MaxRedirects}
		}
		next, err := resolveRedirect(current, loc)
		if err != nil {
			audit.Reason = err.Error()
			c.emit(audit)
			return nil, err
		}
		// 303 或 301/302 的 POST → GET（与 net/http 语义对齐）。
		if resp.StatusCode == http.StatusSeeOther ||
			((resp.StatusCode == http.StatusMovedPermanently || resp.StatusCode == http.StatusFound) &&
				req.Method != http.MethodGet && req.Method != http.MethodHead) {
			req = cloneRequest(req, http.MethodGet, nil)
			body = nil
		}
		current = next
	}
}

// round 发单跳请求（禁用 net/http 自带重定向——重定向由 Do 逐跳复核）。
func (c *NetClient) round(ctx context.Context, req *vshnet.Request, rawURL string, body []byte) (*http.Response, error) {
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = c.cfg.Timeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	transport := &http.Transport{
		// 私网阻断双保险：目标检查（checkTarget）之外，dial 时对实际连接
		// 地址再判一次——DNS rebinding（校验与连接解析结果不同）在此兜底。
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err == nil {
				if ip, perr := netip.ParseAddr(host); perr == nil && ssrfBlockedIP(ip) {
					return nil, &vshnet.AccessDeniedError{URL: rawURL, Reason: "dial to loopback/private/link-local address blocked"}
				}
			}
			if c.cfg.DialContext != nil {
				return c.cfg.DialContext(ctx, network, addr)
			}
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	var rdr io.Reader
	if len(body) > 0 {
		rdr = bytes.NewReader(body)
	}
	hreq, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return nil, err
	}
	for k, v := range req.Headers {
		hreq.Header.Set(k, v)
	}
	return client.Do(hreq)
}

// checkTarget 规则表门 + 私网阻断（初始 URL 与每次重定向目标均过此门）。
func (c *NetClient) checkTarget(ctx context.Context, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return &vshnet.AccessDeniedError{URL: rawURL, Reason: "invalid url"}
	}
	host := vbox.NormalizeHost(u.Hostname())
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	// 规则表（首命中生效；default open——grant net 动态行插表头）。
	if c.cfg.Rules != nil {
		if !c.cfg.Rules().Match(net.JoinHostPort(host, port)) {
			return &vshnet.AccessDeniedError{URL: rawURL, Reason: "denied by net rule table（如需访问请 grant net " + net.JoinHostPort(host, port) + "）"}
		}
	}
	// 私网阻断（显式清单：RFC1918 + loopback + link-local 169.254.0.0/16——
	// 含云 metadata 169.254.169.254，SSRF 首选目标）。
	if ip, err := netip.ParseAddr(host); err == nil {
		if ssrfBlockedIP(ip) {
			return &vshnet.AccessDeniedError{URL: rawURL, Reason: "loopback/private/link-local address blocked"}
		}
		return nil
	}
	resolver := c.cfg.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	ips, err := resolver.LookupIPAddr(ctx, host)
	if err != nil || len(ips) == 0 {
		return fmt.Errorf("cannot resolve %s", host)
	}
	for _, ipa := range ips {
		if addr, ok := netip.AddrFromSlice(ipa.IP); ok && ssrfBlockedIP(addr) {
			return &vshnet.AccessDeniedError{URL: rawURL, Reason: host + " resolves to a disallowed address (loopback/private/link-local)"}
		}
	}
	return nil
}

// readBody 配额预检 + 响应上限截断读。
func (c *NetClient) readBody(resp *http.Response, rawURL string) ([]byte, error) {
	if resp.ContentLength > 0 && c.cfg.OnResponseSize != nil {
		if err := c.cfg.OnResponseSize(resp.ContentLength); err != nil {
			return nil, err
		}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, c.cfg.MaxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > c.cfg.MaxResponseBytes {
		return nil, &vshnet.ResponseTooLargeError{MaxBytes: c.cfg.MaxResponseBytes}
	}
	return data, nil
}

func (c *NetClient) emit(a NetAudit) {
	if c.cfg.Audit != nil {
		c.cfg.Audit(a)
	}
}

// ssrfBlockedIP 私网阻断判定（loopback/RFC1918/link-local/unspecified/multicast）。
func ssrfBlockedIP(ip netip.Addr) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

func isRedirectStatus(code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

func resolveRedirect(currentURL, location string) (string, error) {
	base, err := url.Parse(currentURL)
	if err != nil {
		return "", err
	}
	ref, err := url.Parse(location)
	if err != nil {
		return "", err
	}
	return base.ResolveReference(ref).String(), nil
}

func cloneRequest(req *vshnet.Request, method string, body []byte) *vshnet.Request {
	out := *req
	out.Method = method
	out.Body = body
	return &out
}

func flattenHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, vs := range h {
		out[k] = strings.Join(vs, ", ")
	}
	return out
}

var _ vshnet.Client = (*NetClient)(nil)
