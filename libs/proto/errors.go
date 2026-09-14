package proto

import (
	"errors"
	"fmt"
)

// 错误模型（§2.3）：执行错误 / 拒绝 / 需审批 三类，所有环境统一。

// ExecError 执行错误（state=error）：指令已执行但失败——文件不存在、
// 命令非零退出等。消息格式：{工具} {action}: {原因}。
type ExecError struct {
	Tool   string // fs | exec
	Action string
	Reason string
}

func (e *ExecError) Error() string {
	if e.Tool == "" {
		// 虚拟指令错误：{cmd}: {原因}（§5.4，指令名本身即 action）
		return fmt.Sprintf("%s: %s", e.Action, e.Reason)
	}
	if e.Action == "" {
		return fmt.Sprintf("%s: %s", e.Tool, e.Reason)
	}
	return fmt.Sprintf("%s %s: %s", e.Tool, e.Action, e.Reason)
}

// DeniedError 策略明确拒绝（state=rejected）：路径越界、1host 非法、
// LevelNone、caps 未启用。不可通过审批绕过。
type DeniedError struct{ Reason string }

func (e *DeniedError) Error() string { return e.Reason }

// ApprovalError 权限不足但可审批（state=waiting）：
// 审批通过后以 granted_level=9 重发，执行端只做数字比较，无 fingerprint。
type ApprovalError struct {
	Reason  string
	Preview string
}

func (e *ApprovalError) Error() string { return e.Reason }

// StateOf 将错误映射为协议层状态（§6.2）：DeniedError → rejected，
// ApprovalError → waiting，其余一律执行错误 error。
func StateOf(err error) State {
	var de *DeniedError
	if errors.As(err, &de) {
		return StateRejected
	}
	var ae *ApprovalError
	if errors.As(err, &ae) {
		return StateWaiting
	}
	return StateError
}

// StrategyError 返回错误链中的策略类错误（*ApprovalError / *DeniedError），
// 非策略错误返回 nil。执行链在包装错误（%s 拍平、拼文案）之前必须先调用——
// 策略错误必须保型上抛：审批/拒绝语义靠类型判定（StateOf / 信封 / 审批流），
// 拍平成执行错误即丢失（cloud 内联 vcore 的 GatedFS 写分级兜底曾因此不弹审批）。
func StrategyError(err error) error {
	var ae *ApprovalError
	if errors.As(err, &ae) {
		return ae
	}
	var de *DeniedError
	if errors.As(err, &de) {
		return de
	}
	return nil
}
