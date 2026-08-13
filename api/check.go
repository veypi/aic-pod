package api

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/veypi/vigo"
)

// CheckHostReq 是 check_host 的请求：探测平台地址是否可达。
type CheckHostReq struct {
	Host string `json:"host" src:"json"`
}

// CheckHostResp 返回探测结果与探测用的完整 URL。
type CheckHostResp struct {
	OK  bool   `json:"ok"`
	URL string `json:"url"`
}

// CheckHost 探测 {host}/root.html 是否可获取（vhtml 入口页，200 即可达）。
// 用途：本地设置页保存 host 后验证平台可达再跳转（Electron 主进程 / 浏览器壳共用；
// 主进程另有 IPC 版本，走 net 请求避免 CORS）。
func CheckHost(x *vigo.X, req *CheckHostReq) (*CheckHostResp, error) {
	base := strings.TrimSpace(req.Host)
	if base == "" {
		return nil, vigo.ErrInvalidArg.WithString("host is empty")
	}
	if !strings.Contains(base, "://") {
		base = "https://" + base
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, vigo.ErrInvalidArg.WithString("invalid host (http/https only)")
	}
	target := strings.TrimSuffix(base, "/") + "/root.html"
	client := &http.Client{Timeout: 5 * time.Second}
	r, err := client.Get(target)
	if err != nil {
		return &CheckHostResp{OK: false, URL: target}, nil
	}
	defer r.Body.Close()
	return &CheckHostResp{OK: r.StatusCode >= 200 && r.StatusCode < 400, URL: target}, nil
}
