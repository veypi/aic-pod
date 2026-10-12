package protocol

import (
	"fmt"
	"strings"
)

// 执行目标（§1.1）：1host 的合法取值中无需解析为物理 host 的两个保留字。
const (
	HostCloud = "cloud"
	HostPage  = "page"
)

// HostIDPrefix 是**会话级 subject 中物理 host 段**的强制前缀（§6.1）。它是安全
// 边界的一部分：前端 JWT 的 sub deny 规则（FrontendDenyPattern）依赖此前缀精确
// 匹配物理 host 的工具流量而不误伤 h.page.> 与其他会话 topic。
//
// host_id 本身**不带**前缀：1host 参数、连接级 caps/presence subject、host list
// 输出均用裸 id；仅会话级工具流量（ToolReqSubject/HostInboxSubject）的 host 段
// 在构造时添加（hostSeg），解析时还原（ParseToolReqSubject）。
// 改动必须同步 natsauth 的 host JWT sub allow（issueUserJWT，经 HostInboxSubject）。
const HostIDPrefix = "host_"

// validSeg 校验 subject 段：非空且不含 . * > 与空白。
func validSeg(s string) bool {
	return s != "" && !strings.ContainsAny(s, ".*> \t\r\n")
}

// ---- 连接级 subject（host 生命周期，§6.1） ----

// CapsSubject 能力声明：u.{uid}.h.{host_id}.{cred_ver}.caps
func CapsSubject(uid, hostID string, credVer uint64) (string, error) {
	return connSubject(uid, hostID, credVer, "caps")
}

// PresenceSubject 心跳：u.{uid}.h.{host_id}.{cred_ver}.presence
func PresenceSubject(uid, hostID string, credVer uint64) (string, error) {
	return connSubject(uid, hostID, credVer, "presence")
}

func connSubject(uid, hostID string, credVer uint64, suffix string) (string, error) {
	if !validSeg(uid) || !validSeg(hostID) {
		return "", fmt.Errorf("proto: invalid uid/hostID segment")
	}
	if credVer == 0 {
		return "", fmt.Errorf("proto: credVer must be >= 1")
	}
	return fmt.Sprintf("u.%s.h.%s.%d.%s", uid, hostID, credVer, suffix), nil
}

// ---- 工具请求 subject（连接级，§6.1 v3） ----

// hostSeg 构造 subject 的 host 段：物理 host 加 HostIDPrefix 前缀，
// page/cloud 保留字与已带前缀的输入原样（幂等）。
func hostSeg(host string) string {
	if host == HostCloud || host == HostPage || strings.HasPrefix(host, HostIDPrefix) {
		return host
	}
	return HostIDPrefix + host
}

// HostInboxSubject 是物理 host 端连接时的单订阅：u.{uid}.h.host_{host_id}.>
// 覆盖该 host 全部工具请求（fs/exec，7 段含 sid 段），无 per-session 订阅 churn。
// host 段自动加 HostIDPrefix（与 ToolReqSubject 同源）。
// 不会匹配 caps/presence（其 host 段为裸 host_id，无前缀）。
func HostInboxSubject(uid, hostID string) (string, error) {
	if !validSeg(uid) || !validSeg(hostID) {
		return "", fmt.Errorf("proto: invalid uid/hostID segment")
	}
	return fmt.Sprintf("u.%s.h.%s.>", uid, hostSeg(hostID)), nil
}

// ---- RTC 信令 subject（2026-09-10，WebRTC 直连，§6.1 扩展） ----

// RtcInSubject RTC 信令入向（页面 → 设备）：u.{uid}.h.host_{host_id}.rtc.in
// host 端通配 inbox（HostInboxSubject）天然覆盖；前端 JWT pub allow
// （u.{uid}.>）覆盖，pub 方向不受 FrontendDenyPattern 限制。
func RtcInSubject(uid, hostID string) (string, error) {
	if !validSeg(uid) || !validSeg(hostID) {
		return "", fmt.Errorf("proto: invalid uid/hostID segment")
	}
	return fmt.Sprintf("u.%s.h.%s.rtc.in", uid, hostSeg(hostID)), nil
}

// RtcOutSubject RTC 信令出向（设备 → 页面）：u.{uid}.h.{host_id}.{cred_ver}.rtc
// 与 caps/presence 同族（bare host_id 段）——不在前端 JWT sub deny（host_ 前缀
// 通配）范围内，页面可订阅；host JWT pub allow 需显式放行（natsauth）。
func RtcOutSubject(uid, hostID string, credVer uint64) (string, error) {
	return connSubject(uid, hostID, credVer, "rtc")
}

// ---- 前端 JWT 权限模板（§6.1） ----

// UserAllowPattern 是前端 JWT 的 pub/sub allow：u.{uid}.>
func UserAllowPattern(uid string) (string, error) {
	if !validSeg(uid) {
		return "", fmt.Errorf("proto: invalid uid segment")
	}
	return fmt.Sprintf("u.%s.>", uid), nil
}

// FrontendDenyPattern 是前端 JWT 的 sub deny：u.{uid}.h.host_*.>
// 连接级物理 host 工具流量（含文件内容、审批事实）不对浏览器端可观测——
// 浏览器是注入风险最高的端，签名防伪造不防旁观；host_ 前缀精确匹配物理
// host（连接级 subject，§6.1 v3），page 主题（h.page）不受影响。
func FrontendDenyPattern(uid string) (string, error) {
	if !validSeg(uid) {
		return "", fmt.Errorf("proto: invalid uid segment")
	}
	return fmt.Sprintf("u.%s.h.%s*.>", uid, HostIDPrefix), nil
}
