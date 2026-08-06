package api

import (
	"net/url"
	"strings"

	"github.com/veypi/aic-pod/libs/utils"
	"github.com/veypi/vigo"
)

// openURLReq 是 open_url/open_platform 的请求。
type openURLReq struct {
	URL string `json:"url" src:"json"`
}

// parseHTTPURL 校验仅接受 http/https（防本地命令注入）。
func parseHTTPURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, vigo.ErrInvalidArg.WithString("invalid url (http/https only)")
	}
	return u, nil
}

// OpenURL 用系统默认浏览器打开外链（desktop 外链委托，2026-08-06）：
// 平台窗口是 WKWebView（mac 未实现 createWebView，target=_blank 外链点击无反应），
// 前端 local_handler.js 拦截外链点击经此端点交给系统浏览器（自带多标签）。
// origin 校验在前端完成。
func OpenURL(x *vigo.X, req *openURLReq) (*OKResp, error) {
	u, err := parseHTTPURL(req.URL)
	if err != nil {
		return nil, err
	}
	if err := utils.OpenExternal(u.String()); err != nil {
		return nil, vigo.ErrInternalServer.WithError(err)
	}
	return &OKResp{OK: true}, nil
}

// OpenPlatform 应用内打开平台 URL（OpenPlatformURL 变量：desktop 窗口跳转；
// cli 默认系统浏览器）。
func OpenPlatform(x *vigo.X, req *openURLReq) (*OKResp, error) {
	u, err := parseHTTPURL(req.URL)
	if err != nil {
		return nil, err
	}
	if err := OpenPlatformURL(u.String()); err != nil {
		return nil, vigo.ErrInternalServer.WithError(err)
	}
	return &OKResp{OK: true}, nil
}
