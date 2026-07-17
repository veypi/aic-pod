// Package aicenv 提供 AIC Env 协议的 Go 实现。
// 客户端通过此 SDK 可快速将设备接入 AIC 平台，注册执行能力并处理工具请求。
package aicenv

import "context"

// Options 客户端配置。
type Options struct {
	Credential string // "<env_id>.<cred_ver>.<secret>.<uid>" (必填)
	NATSURL    string // "nats://host:port" 或 "ws://host/path" (必填)
	WorkDir    string // exec 工作目录，默认 /tmp
	DeviceName string // 展示名称，默认 hostname
	DeviceType string // sandbox / device / server
	Version    string // 客户端版本号
	OnLog      func(format string, args ...any)
}

// ToolDef 工具定义，对应 CAPS 中的 tool 条目。
type ToolDef struct {
	Name          string         `json:"name"`
	Description   string         `json:"description"`
	Parameters    map[string]any `json:"parameters,omitempty"`
	RequiredLevel int            `json:"required_level"`
	PolicyVersion string         `json:"policy_version"`
}

// ToolHandler 工具处理函数。
// ctx 中可通过 RequestFromContext 获取请求上下文（权限等级、审批状态）。
// toolData 是服务端发来的原始参数，字段由工具自行定义。
type ToolHandler func(ctx context.Context, toolData any) (*ToolResult, error)

// Tool 一个可注册的工具。
type Tool struct {
	Def     ToolDef
	Handler ToolHandler
}

// ToolParams 是标准 action + argv 参数结构。
type ToolParams struct {
	Action string   `json:"action"`
	Argv   []string `json:"argv"`
}

// ToolResult 工具执行结果。
type ToolResult struct {
	Status       string              `json:"status,omitempty"` // ""="completed", "waiting", "rejected"
	Content      string              `json:"content,omitempty"`
	Error        string              `json:"error,omitempty"`
	Attrs        map[string]string   `json:"attrs,omitempty"`
	NeedApproval *NeedApprovalDetail `json:"need_approval,omitempty"`
}

// NeedApprovalDetail 是执行中动态审批的详情。
type NeedApprovalDetail struct {
	Reason      string `json:"reason"`
	WaitType    string `json:"wait_type,omitempty"`
	Preview     string `json:"preview,omitempty"`
	Fingerprint string `json:"fingerprint"`
}

// RequestCtx 是工具请求上下文，通过 context 传递给 handler。
type RequestCtx struct {
	GrantedLevel int
	Approved     bool
	ResolvedBy   string
	SessionID    string
	MsgID        string
}

type reqCtxKey struct{}

// RequestFromContext 从 context 获取请求上下文。
func RequestFromContext(ctx context.Context) *RequestCtx {
	if v, ok := ctx.Value(reqCtxKey{}).(*RequestCtx); ok {
		return v
	}
	return nil
}

// ---- 内部类型 ----

type capabilities struct {
	EnvID         string         `json:"env_id"`
	AgentVersion  string         `json:"agent_version"`
	CredentialVer uint64         `json:"credential_ver"`
	DeviceType    string         `json:"device_type,omitempty"`
	DeviceName    string         `json:"device_name,omitempty"`
	DeviceInfo    *envDeviceInfo `json:"device_info,omitempty"`
	Tools         []ToolDef      `json:"tools"`
}

type envDeviceInfo struct {
	Hostname  string `json:"hostname"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	NumCPU    int    `json:"num_cpu"`
	GoVersion string `json:"go_version"`
}

type toolRequest struct {
	MsgID         string `json:"msg_id"`
	SessionID     string `json:"session_id"`
	ToolName      string `json:"tool_name"`
	ToolData      any    `json:"tool_data"`
	GrantedLevel  int    `json:"granted_level"`
	Signature     string `json:"sig,omitempty"`
	Nonce         string `json:"nonce,omitempty"`
	Deadline      string `json:"deadline,omitempty"`
	Approval      *struct {
		Fingerprint string `json:"fingerprint"`
		ResolvedBy  string `json:"resolved_by"`
	} `json:"approval,omitempty"`
}

type toolResponse struct {
	MsgID        string              `json:"msg_id"`
	Status       string              `json:"status"`
	Content      string              `json:"content,omitempty"`
	Error        string              `json:"error,omitempty"`
	Attrs        map[string]string   `json:"attrs,omitempty"`
	NeedApproval *NeedApprovalDetail `json:"need_approval,omitempty"`
}

type connectSigPayload struct {
	Domain  string `json:"domain"`
	EnvID   string `json:"env_id"`
	UID     string `json:"uid"`
	EnvInfo string `json:"env_info"`
	UnixMS  int64  `json:"unix_ms"`
	Nonce   string `json:"nonce"`
}

type toolReqSigPayload struct {
	Version             int     `json:"version"`
	EnvID               string  `json:"env_id"`
	MsgID               string  `json:"msg_id"`
	SessionID           string  `json:"session_id"`
	ToolName            string  `json:"tool_name"`
	ToolDataSHA256      string  `json:"tool_data_sha256"`
	GrantedLevel        int     `json:"granted_level"`
	Nonce               string  `json:"nonce"`
	Deadline            string  `json:"deadline"`
	ApprovalFingerprint *string `json:"approval_fingerprint,omitempty"`
}
