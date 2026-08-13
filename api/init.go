// Copyright (C) 2025 veypi <i@veypi.com>
// Distributed under terms of the MIT license.

// Package api 是本地管理端点集合（/api/*，由根包 Extend 挂载）。
// 调用方：本地壳页面（同源，$mod.$fetch 自动注入 x-aic-code 头，code 为纯秘钥）；
// Chrome 插件经 content script 桥（ext.{hex}）。所有端点请求须带 x-aic-code 头。
// 安全边界：
//   - 仅监听 127.0.0.1 随机端口（不对外）
//   - Origin 白名单：仅平台源响应 CORS 头（含 Private Network Access 预检）
//   - code 由进程随机生成（cfg.NewOptions，可配置写死）；连续 5 次校验失败锁 1 分钟
package api

import (
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/vigo"
	"github.com/veypi/vigo/contrib/common"
)

// Router 本地管理端点：security 中间件（CORS 白名单/PNA + code 校验）+
// 统一 JSON 响应（common.JsonResponse/JsonErrorResponse）。
var Router = vigo.NewRouter().EnableApiDoc().Use(security).After(common.JsonResponse, common.JsonErrorResponse)

func init() {
	Router.Get("/ping", "Ping", Ping)
	Router.Get("/get_config", "Get Config", GetConfig)
	Router.Post("/set_config", "Set Config", SetConfig)
	Router.Post("/bind", "Bind", Bind)
	Router.Post("/unbind", "Unbind", Unbind)
	Router.Get("/get_status", "Get Status", GetStatus)
	Router.Get("/get_log", "Get Log", GetLog)
	Router.Post("/start", "Start Host", StartHost)
	Router.Post("/stop", "Stop Host", StopHost)
	Router.Post("/check_host", "Check Host", CheckHost)
	// 兜底：OPTIONS 预检由 security 统一短路（vigo 路由未命中不走 Use 链，
	// 必须能 match 到路由）；其余未注册路径返回 JSON 404。
	Router.Any("/**", "Catch All", CatchAll)
}

// CatchAll 兜底 handler（OPTIONS 到不了这里——security 已 x.Stop()）。
func CatchAll(x *vigo.X) (any, error) {
	return nil, vigo.ErrNotFound
}

// OKResp 是写操作端点的统一确认返回。
type OKResp struct {
	OK bool `json:"ok"`
}

// mu 保护 cfg.Global 写操作与 code 校验计数（failCnt/lockEnd）。
var mu sync.Mutex

// effective 返回当前有效配置（值拷贝）。
func effective() cfg.Options {
	mu.Lock()
	defer mu.Unlock()
	return *cfg.Global
}

// ---- 安全中间件 ----

var (
	failCnt int
	lockEnd time.Time
)

// defaultOrigins 是默认允许跨域访问本地服务的平台源。
// "null" 为嵌入式设置窗口（旧 wails SetHTML 场景，origin 为 null）——受 code 校验保护。
func defaultOrigins() []string {
	return []string{
		"https://ivec.ai",
		"http://localhost:4000",
		"http://127.0.0.1:4000",
		"null",
	}
}

// allowedOrigins 实际白名单 = 默认列表 + 当前有效配置 host 的 origin
// （启动 flag/env 注入的测试地址等自动放行）。
func allowedOrigins() []string {
	list := defaultOrigins()
	if cfg.Global.Host != "" {
		if u, err := url.Parse(cfg.Global.Host); err == nil && u.Scheme != "" && u.Host != "" {
			origin := fmt.Sprintf("%s://%s", u.Scheme, u.Host)
			for _, x := range list {
				if x == origin {
					return list
				}
			}
			list = append(list, origin)
		}
	}
	return list
}

// security 是安全中间件：CORS 白名单 + OPTIONS 预检短路 + code 校验
// （/api/ping 无需凭证）。校验失败写响应后 x.Stop() 终止后续 handler。
func security(x *vigo.X) error {
	r := x.Request
	w := x.ResponseWriter()
	origin := r.Header.Get("Origin")
	allowed := ""
	for _, o := range allowedOrigins() {
		if o == origin {
			allowed = o
			break
		}
	}
	// 预检：仅白名单 origin 响应 CORS + PNA 头
	if r.Method == http.MethodOptions {
		if allowed == "" {
			http.Error(w, "forbidden origin", http.StatusForbidden)
		} else {
			w.Header().Set("Access-Control-Allow-Origin", allowed)
			w.Header().Set("Access-Control-Allow-Private-Network", "true")
			w.Header().Set("Access-Control-Allow-Headers", "x-aic-code, content-type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.WriteHeader(http.StatusNoContent)
		}
		x.Stop()
		return nil
	}
	if allowed != "" {
		w.Header().Set("Access-Control-Allow-Origin", allowed)
		w.Header().Set("Access-Control-Allow-Private-Network", "true")
	}
	if r.URL.Path != "/api/ping" && !checkCode(r) {
		http.Error(w, "invalid or expired local code", http.StatusUnauthorized)
		x.Stop()
		return nil
	}
	return nil
}

// checkCode 校验 x-aic-code：存在、未过期、未锁定；失败计数达 5 次锁 1 分钟。
func checkCode(r *http.Request) bool {
	mu.Lock()
	defer mu.Unlock()
	now := time.Now()
	if now.Before(lockEnd) {
		return false
	}
	if r.Header.Get("x-aic-code") != cfg.Global.Code {
		failCnt++
		if failCnt >= 5 {
			lockEnd = now.Add(time.Minute)
			failCnt = 0
		}
		return false
	}
	failCnt = 0
	return true
}
