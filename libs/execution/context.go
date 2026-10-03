package execution

import (
	"context"
	"time"
)

// sessionFSKey 把每会话 FS 经 ctx 传给 Runtime 的 fs.Factory（Runtime 单例，
// Factory.New(ctx) 在 NewSession 时调用——以此把会话身份带进工厂）。
type sessionFSKey struct{}

// netSessionKey 把会话键经 ctx 传给 NetClient（规则表 per-session 快照，
// cloud 多用户进程不可用进程级并集——跨用户泄漏授权）。
type netSessionKey struct{}

// ownerKey 把任务归属（用户身份）经 ctx 透传。
type ownerKey struct{}

// grantApprovedKey 携带服务端确认过的 grant 审批事实（可信上下文；
// 不进入 argv/env——脚本不可修改）。
type grantApprovedKey struct{}

// noSandboxKey 携带本次执行的免沙箱选项（可信上下文）。
type noSandboxKey struct{}

// waitBudgetKey 携带前台等待截止（bg wait 的共享预算源）。
type waitBudgetKey struct{}

// execHandleKey 携带本次执行的句柄（bg wait 禁止等待自身用）。
type execHandleKey struct{}

// OwnerFromContext 取 Exec 注入的任务归属（无注入 = 空串匿名共池）。
func OwnerFromContext(ctx context.Context) string {
	o, _ := ctx.Value(ownerKey{}).(string)
	return o
}

// SessionFromContext 取 Exec 注入的会话键（NetClient 规则表快照源用；
// 无注入 = 空串——快照退化为无 temp 行的基表）。
func SessionFromContext(ctx context.Context) string {
	sid, _ := ctx.Value(netSessionKey{}).(string)
	return sid
}

// GrantApprovedFromContext 取本次执行的 grant 审批事实（默认 false）。
func GrantApprovedFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(grantApprovedKey{}).(bool)
	return v
}

// NoSandboxFromContext 取本次执行的免沙箱选项（默认 false）。
func NoSandboxFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(noSandboxKey{}).(bool)
	return v
}

// WaitBudgetRemaining 取剩余前台等待预算（bg wait 用）：
// 返回 min(剩余预算减 1 秒) 与 ok；无预算（后台执行/未注入）或预算耗尽
// 时 ok=false——此时 wait 只查询不阻塞。
func WaitBudgetRemaining(ctx context.Context) (time.Duration, bool) {
	deadline, ok := ctx.Value(waitBudgetKey{}).(time.Time)
	if !ok {
		return 0, false
	}
	remain := time.Until(deadline) - time.Second
	if remain <= 0 {
		return 0, false
	}
	return remain, true
}

// HandleFromContext 取本次执行的句柄（bg wait 禁止等待自身用）。
func HandleFromContext(ctx context.Context) *ExecHandle {
	h, _ := ctx.Value(execHandleKey{}).(*ExecHandle)
	return h
}
