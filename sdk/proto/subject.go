package proto

import (
	"fmt"
	"strings"
)

// 执行目标（§1.1）：1host 的合法取值中无需解析为物理 host 的两个保留字。
const (
	HostCloud = "cloud"
	HostPage  = "page"
)

// 指令集名（§6.2）：信封 tool 字段与 subject 段一致。
const (
	ToolFS   = "fs"
	ToolExec = "exec"
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

// ToolReqSubject 工具请求（连接级，§6.1 v3）：u.{uid}.h.{host}.{tool}.req
// 执行不绑定会话——session_id 不进 subject（由信封 SessionID 携带，供权限/命名/落盘）。
// host 段：物理 host = host_{host_id}（HostIDPrefix，构造时自动添加）；
// page/cloud 保留字原样（1host 寻址同构）。cloud 服务端本地执行，不过 NATS。
func ToolReqSubject(uid, host, tool string) (string, error) {
	if !validSeg(uid) || !validSeg(host) {
		return "", fmt.Errorf("proto: invalid uid/host segment")
	}
	if tool != ToolFS && tool != ToolExec {
		return "", fmt.Errorf("proto: invalid tool %q (must be fs|exec)", tool)
	}
	return fmt.Sprintf("u.%s.h.%s.%s.req", uid, hostSeg(host), tool), nil
}

// FSReqSubject 是 ToolReqSubject 的 fs 简写。
func FSReqSubject(uid, host string) (string, error) {
	return ToolReqSubject(uid, host, ToolFS)
}

// ExecReqSubject 是 ToolReqSubject 的 exec 简写。
func ExecReqSubject(uid, host string) (string, error) {
	return ToolReqSubject(uid, host, ToolExec)
}

// HostInboxSubject 是物理 host 端连接时的单订阅：u.{uid}.h.host_{host_id}.>
// 覆盖该 host 全部工具请求（fs/exec，连接级），无 per-session 订阅 churn。
// host 段自动加 HostIDPrefix（与 ToolReqSubject 同源）。
// 不会匹配 caps/presence（其 host 段为裸 host_id，无前缀）。
func HostInboxSubject(uid, hostID string) (string, error) {
	if !validSeg(uid) || !validSeg(hostID) {
		return "", fmt.Errorf("proto: invalid uid/hostID segment")
	}
	return fmt.Sprintf("u.%s.h.%s.>", uid, hostSeg(hostID)), nil
}

// ParseToolReqSubject 解析连接级工具请求 subject（6 段）：
// u.{uid}.h.{host}.{tool}.req → uid, host(还原裸 id / page 原样), tool。
// session_id 不在 subject 中（信封 SessionID 携带）。
func ParseToolReqSubject(subject string) (uid, host, tool string, err error) {
	parts := strings.Split(subject, ".")
	// u.{uid}.h.{host}.{tool}.req 恰好 6 段
	if len(parts) != 6 || parts[0] != "u" || parts[2] != "h" || parts[5] != "req" {
		return "", "", "", fmt.Errorf("proto: not a tool request subject: %s", subject)
	}
	if parts[4] != ToolFS && parts[4] != ToolExec {
		return "", "", "", fmt.Errorf("proto: invalid tool segment in subject: %s", subject)
	}
	return parts[1], strings.TrimPrefix(parts[3], HostIDPrefix), parts[4], nil
}

// PageQueueGroup 是 page 端 fs.req/exec.req 的统一 queue group（§6.1 v3）：page-{uid}。
// 连接级单订阅下同一用户的所有 tab 进同一组，NATS 保证恰好投递一个接收者
//（避免多 tab fan-out 重复执行）；sub 白名单在页面内查表过滤。
func PageQueueGroup(uid string) string { return "page-" + uid }

// ---- 前端 JWT 权限模板（§6.1） ----

// UserAllowPattern 是前端 JWT 的 pub/sub allow：u.{uid}.>
func UserAllowPattern(uid string) (string, error) {
	if !validSeg(uid) {
		return "", fmt.Errorf("proto: invalid uid segment")
	}
	return fmt.Sprintf("u.%s.>", uid), nil
}

// FrontendDenyPattern 是前端 JWT 的 sub deny：u.{uid}.h.host_*.>
// 连接级物理 host 工具流量（含文件内容、granted_level）不对浏览器端可观测——
// 浏览器是注入风险最高的端，签名防伪造不防旁观；host_ 前缀精确匹配物理
// host（连接级 subject，§6.1 v3），page 主题（h.page）不受影响。
func FrontendDenyPattern(uid string) (string, error) {
	if !validSeg(uid) {
		return "", fmt.Errorf("proto: invalid uid segment")
	}
	return fmt.Sprintf("u.%s.h.%s*.>", uid, HostIDPrefix), nil
}
