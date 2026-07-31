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

// HostIDPrefix 是 host_id 的强制前缀。它是安全边界的一部分：
// 前端 JWT 的 sub deny 规则（FrontendDenyPattern）依赖此前缀精确匹配物理 host
// 而不误伤 h.page.> 与其他会话 topic。改动必须同步 natsauth。
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

// ---- 会话级 subject（工具流量，§6.1） ----

// ToolReqSubject 工具请求：u.{uid}.s.{sid}.h.{host}.{fs|exec}.req
// host 段填 host_id 或 page（1host 寻址同构）；cloud 服务端本地执行，不过 NATS。
func ToolReqSubject(uid, sid, host, tool string) (string, error) {
	if !validSeg(uid) || !validSeg(sid) || !validSeg(host) {
		return "", fmt.Errorf("proto: invalid uid/sid/host segment")
	}
	if tool != ToolFS && tool != ToolExec {
		return "", fmt.Errorf("proto: invalid tool %q (must be fs|exec)", tool)
	}
	return fmt.Sprintf("u.%s.s.%s.h.%s.%s.req", uid, sid, host, tool), nil
}

// FSReqSubject 是 ToolReqSubject 的 fs 简写。
func FSReqSubject(uid, sid, host string) (string, error) {
	return ToolReqSubject(uid, sid, host, ToolFS)
}

// ExecReqSubject 是 ToolReqSubject 的 exec 简写。
func ExecReqSubject(uid, sid, host string) (string, error) {
	return ToolReqSubject(uid, sid, host, ToolExec)
}

// HostInboxSubject 是 host 端连接时的单订阅：u.{uid}.s.*.h.{host_id}.>
// 覆盖该 host 所有会话的 fs/exec 请求，无 per-session 订阅 churn。
func HostInboxSubject(uid, hostID string) (string, error) {
	if !validSeg(uid) || !validSeg(hostID) {
		return "", fmt.Errorf("proto: invalid uid/hostID segment")
	}
	return fmt.Sprintf("u.%s.s.*.h.%s.>", uid, hostID), nil
}

// ParseToolReqSubject 解析会话级工具请求 subject（host 端从 subject 取 sid
// 做会话隔离——bg 注册表 {host}:{sid}:{op_id} 命名空间与 topic 一致）。
func ParseToolReqSubject(subject string) (uid, sid, host, tool string, err error) {
	parts := strings.Split(subject, ".")
	// u.{uid}.s.{sid}.h.{host}.{tool}.req 恰好 8 段
	if len(parts) != 8 || parts[0] != "u" || parts[2] != "s" || parts[4] != "h" || parts[7] != "req" {
		return "", "", "", "", fmt.Errorf("proto: not a tool request subject: %s", subject)
	}
	if parts[6] != ToolFS && parts[6] != ToolExec {
		return "", "", "", "", fmt.Errorf("proto: invalid tool segment in subject: %s", subject)
	}
	return parts[1], parts[3], parts[5], parts[6], nil
}

// PageQueueGroup 是 page 端 fs.req/exec.req 的统一 queue group（§6.1）：page-{sid}。
// 前端按可见性动态进出组：tab 变 visible 时订阅进组、变 hidden 时退订出组，
// NATS 保证恰好投递一个可见 tab。
func PageQueueGroup(sid string) string { return "page-" + sid }

// ---- 前端 JWT 权限模板（§6.1） ----

// UserAllowPattern 是前端 JWT 的 pub/sub allow：u.{uid}.>
func UserAllowPattern(uid string) (string, error) {
	if !validSeg(uid) {
		return "", fmt.Errorf("proto: invalid uid segment")
	}
	return fmt.Sprintf("u.%s.>", uid), nil
}

// FrontendDenyPattern 是前端 JWT 的 sub deny：u.{uid}.s.*.h.host_*.>
// 物理 host 的工具流量（含文件内容、granted_level）不对浏览器端可观测——
// 浏览器是注入风险最高的端，签名防伪造不防旁观。
func FrontendDenyPattern(uid string) (string, error) {
	if !validSeg(uid) {
		return "", fmt.Errorf("proto: invalid uid segment")
	}
	return fmt.Sprintf("u.%s.s.*.h.%s*.>", uid, HostIDPrefix), nil
}
