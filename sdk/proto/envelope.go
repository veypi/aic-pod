package proto

import "encoding/json"

// State 是协议层响应状态（§6.2），与 Attrs 内的 status 键（§2.2，取值
// error/rejected）明确区分，禁止混用。
type State string

const (
	StateCompleted State = "completed" // 成功，content+attrs 即 §2.2 结构
	StateWaiting   State = "waiting"   // host 端动态审批，携带 NeedApproval
	StateRejected  State = "rejected"  // host 端策略拒绝（纵深防御命中）
	StateError     State = "error"     // 执行失败，content 可保留部分输出
)

// ToolRequest 是 server→host 的工具请求信封（§6.2，req-reply，HMAC-SHA256 签名）。
//
// Data 为指令集原生负载：fs = JSON 参数全体（含 action/path/...）；
// exec = {"action","argv","workdir"?}。路径展开语义按 §2.1.1：fs 参数与虚拟指令
// 路径参数为展开后绝对形式；程序命令 argv 原样透传 + workdir 同信封下发。
type ToolRequest struct {
	MsgID        string          `json:"msg_id"`
	SessionID    string          `json:"session_id"`
	Tool         string          `json:"tool"` // fs | exec
	Data         json.RawMessage `json:"data"`
	GrantedLevel int             `json:"granted_level"`
	Nonce        string          `json:"nonce"`
	Deadline     string          `json:"deadline"` // RFC3339，服务端按 §2.5 换算
	Sig          string          `json:"sig"`
	// HostID 不随信封传输（subject 已编码），仅参与签名输入；收发两端各自持有。
	HostID string `json:"-"`
}

// NeedApproval 是 host 端动态审批详情（state=waiting 时携带）。
// 审批通过后服务端以 granted_level=9 重发（同 msg_id、新 nonce/deadline），
// host 端纯数字比较。
type NeedApproval struct {
	Reason  string `json:"reason"`
	Preview string `json:"preview,omitempty"`
}

// ToolResponse 是 host→server 的响应信封（§6.2）。
// page 前端只使用 completed/error 子集（§6.1：page 的审批全部在服务端事前完成）。
type ToolResponse struct {
	MsgID        string            `json:"msg_id"`
	State        State             `json:"state"`
	Content      string            `json:"content,omitempty"`
	Error        string            `json:"error,omitempty"`
	Attrs        map[string]string `json:"attrs,omitempty"`
	NeedApproval *NeedApproval     `json:"need_approval,omitempty"`
}
