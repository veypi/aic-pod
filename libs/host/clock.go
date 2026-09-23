package host

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/veypi/aic-pod/cfg"
)

// 时钟校准：设备系统时间可能有任意误差，跨机时效校验（连接 token、工具
// 请求校验窗口、RTC 直连票据）不能依赖系统时间——以平台时钟为基准维护
// 进程级偏移（系统时间 + 偏移 = 平台时间）。每次建立链路前对时一次，长
// 连接期间按 clockSyncInterval 周期重对（系统时钟被 NTP 修正/漂移时刷新）。
// 对时端点为平台公开只读接口 GET /api/time（aic/api/server_time.go）。

// timePath 是平台对时端点挂载路径（服务端 Router.Get("/time")）。
const timePath = "/api/time"

const (
	// clockSyncInterval 周期重对间隔。
	clockSyncInterval = 30 * time.Minute
	// clockHTTPTimeout 单次对时请求超时。
	clockHTTPTimeout = 5 * time.Second
)

var (
	clockOffsetMS   atomic.Int64
	clockSyncWarned atomic.Bool
)

// clockNow 返回校准后的当前时间（平台时间基准）；从未对时成功时退化为
// 系统时间（兼容无 /api/time 的旧平台）。
func clockNow() time.Time {
	if off := clockOffsetMS.Load(); off != 0 {
		return time.Now().Add(time.Duration(off) * time.Millisecond)
	}
	return time.Now()
}

// clockNowMS 返回校准后的 unix 毫秒时间戳。
func clockNowMS() int64 { return clockNow().UnixMilli() }

// setClockOffset 更新校准偏移（毫秒；平台时间 − 本机时间）。
func setClockOffset(offMS int64) { clockOffsetMS.Store(offMS) }

// resolveTimeURL 由平台地址推导对时端点（与 ResolveNATSURL 同源规则：
// ws→http、wss→https、http/https 原样、无 scheme 按 https 补全；
// 产品壳挂载场景保留路径前缀）。
//
//	https://ivec-ai.com            → https://ivec-ai.com/api/time
//	http://127.0.0.1:4000/rses/aiv → http://127.0.0.1:4000/rses/aiv/api/time
func resolveTimeURL(hostURL string) string {
	h := strings.TrimSpace(hostURL)
	if h == "" {
		h = cfg.DefaultHost
	}
	if !strings.Contains(h, "://") {
		h = "https://" + h
	}
	u, err := url.Parse(h)
	if err != nil || u.Host == "" {
		return "https://" + strings.TrimPrefix(h, "https://") + timePath
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	}
	return fmt.Sprintf("%s://%s%s%s", u.Scheme, u.Host, strings.TrimSuffix(u.Path, "/"), timePath)
}

// clockOffsetFrom 计算一次往返得到的偏移：服务端时间戳对应响应发出的时刻，
// 单程延迟按往返一半估计。
func clockOffsetFrom(serverMS int64, t0, t1 time.Time) int64 {
	mid := t0.Add(t1.Sub(t0) / 2)
	return serverMS - mid.UnixMilli()
}

// fetchServerTime 请求平台对时端点，返回服务端当前 unix 毫秒。
func fetchServerTime(hostURL string) (int64, error) {
	client := &http.Client{Timeout: clockHTTPTimeout}
	resp, err := client.Get(resolveTimeURL(hostURL))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("time endpoint status %d", resp.StatusCode)
	}
	var body struct {
		Now int64 `json:"now"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body); err != nil {
		return 0, fmt.Errorf("time endpoint response: %w", err)
	}
	if body.Now <= 0 {
		return 0, fmt.Errorf("time endpoint returned empty timestamp")
	}
	return body.Now, nil
}

// syncClock 向平台对时一次并更新偏移。失败保留旧偏移、不阻断连接；失败
// 日志只在首次出现（成功后重置），避免旧平台/网络故障下反复刷日志。
func (c *Client) syncClock() {
	t0 := time.Now()
	serverMS, err := fetchServerTime(c.options().Host)
	t1 := time.Now()
	if err != nil {
		if clockSyncWarned.CompareAndSwap(false, true) {
			c.logf("clock sync unavailable: %v (fallback to system time)", err)
		}
		return
	}
	clockSyncWarned.Store(false)
	off := clockOffsetFrom(serverMS, t0, t1)
	prev := clockOffsetMS.Load()
	setClockOffset(off)
	if prev != off {
		c.logf("clock calibrated: offset %+dms (round-trip %dms)", off, t1.Sub(t0).Milliseconds())
	}
}
