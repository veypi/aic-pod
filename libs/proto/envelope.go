package proto

// State 是协议层响应状态（§6.2），与 Attrs 内的 status 键（§2.2，取值
// error/rejected）明确区分，禁止混用。
//
// hosts-vsh-redesign（hosts_nats/2）：pod 不再返回 waiting——审批全部在
// 发送前完成，pod 只做身份与 rules 检查；waiting/rejected 常量仅为 page
// 通道历史响应子集保留，host 链路不再产生。
type State string

const (
	StateCompleted State = "completed" // 成功，content+attrs 即 §2.2 结构
	StateWaiting   State = "waiting"   // 历史保留：host 端动态审批（已下线）
	StateRejected  State = "rejected"  // 历史保留：host 端策略拒绝（已下线）
	StateError     State = "error"     // 执行失败，content 可保留部分输出
)

// ToolResponse 是 host/page→server 的响应信封（§6.2/§6.1）。
// page 前端只使用 completed/error 子集（§6.1：page 的审批全部在服务端事前完成）。
type ToolResponse struct {
	MsgID   string            `json:"msg_id"`
	State   State             `json:"state"`
	Content string            `json:"content,omitempty"`
	Error   string            `json:"error,omitempty"`
	Attrs   map[string]string `json:"attrs,omitempty"`
}
