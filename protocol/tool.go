package protocol

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
)

const MaxMessageBytes = 1 << 20

// 请求动作（每个 action 只接受对应载荷）。
const (
	ActionExec   = "exec"
	ActionFS     = "fs"
	ActionCancel = "cancel"
)

// Request 是一条传输无关的工具请求。
type Request struct {
	Protocol string `json:"protocol"`
	ID       string `json:"request_id"`
	Action   string `json:"action"`
	// Exec 是 action=exec 的载荷：完整脚本与执行选项。
	Exec *ExecPayload `json:"exec,omitempty"`
	// FS 是 action=fs 的载荷：数据面方法调用。
	FS *FSInvocation `json:"fs,omitempty"`
	// CancelID 是 action=cancel 的目标 request_id。
	CancelID string `json:"cancel_id,omitempty"`
	// TimeoutMS 是本次请求的传输预算（含排队与回包余量，§2.6）。
	TimeoutMS int64 `json:"timeout_ms,omitempty"`
}

// ExecPayload 执行请求：只传完整脚本。协议不区分脚本里用了哪些指令。
type ExecPayload struct {
	Script  string `json:"script"`
	Workdir string `json:"workdir,omitempty"`
	Stdin   string `json:"stdin,omitempty"`
	// NoSandbox 免沙箱执行（物理 host；发送前审批——批准 nosandbox 不批准 grant）。
	NoSandbox bool `json:"nosandbox,omitempty"`
	// WaitMS 前台等待上限（毫秒）；到期未完成则登记后台并返回 background/id。
	WaitMS int64 `json:"wait_ms,omitempty"`
}

// FSInvocation 是 FS 数据面调用：{method, args} 直达 FS 服务。
type FSInvocation struct {
	Method string          `json:"method"`
	Args   json.RawMessage `json:"args"`
}

// Output 是指令输出（exec 响应结果与 fs 数据面结果同一形状）：Content 为
// 文本正文，Attrs 为结构化元数据（字符串值）。exec 的 attrs 约定：
//   - output / error_output：stdout / stderr 日志地址（日志创建后恒有）
//   - exit_code：完成时存在
//   - truncated：任一预览超限
//   - stderr：stderr 预览
//   - background=true + id：仅等待超时转后台时存在
//
// json tag 同时是 RTC 直连帧协议的线上契约（libs/rtc fschan 直接
// json.Marshal 本类型）——页面端按小写键解析（2026-09-10 实网事故：Go 侧
// 消费方反序列化大小写不敏感，仅 JS 侧可见大写键丢空）。
type Output struct {
	Content string            `json:"content"`
	Attrs   map[string]string `json:"attrs,omitempty"`
}

type Response struct {
	Protocol string `json:"protocol"`
	ID       string `json:"request_id"`
	Result   any    `json:"result,omitempty"`
	Error    *Fault `json:"error,omitempty"`
}
type Fault struct {
	Effect  string `json:"effect,omitempty"`
	Retry   string `json:"retry,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

func (f *Fault) Error() string     { return f.Code + ": " + f.Message }
func Fail(code, msg string) *Fault { return &Fault{Code: code, Message: msg} }

// Coded 是可选错误接口：错误自报 Fault 码，AsFault 在没有现成 *Fault 时据此
// 归类。没有它，调用方参数类错误（如 fs rg context 越界）只能落 internal 兜底，
// 字面像平台内部故障、实际是调用方可自纠的请求问题（2026-10-10 定）。
// 码取自平台既有词表（invalid_argument / unsupported / ...）。
type Coded interface{ FaultCode() string }

func AsFault(err error) *Fault {
	var f *Fault
	if errors.As(err, &f) {
		copy := *f
		return &copy
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Fail("deadline_exceeded", "Call deadline elapsed; effects may already have occurred")
	}
	if errors.Is(err, context.Canceled) {
		return Fail("cancelled", "Call cancelled; effects may already have occurred")
	}
	if errors.Is(err, io.EOF) {
		return Fail("end_of_stream", "Stream ended")
	}
	// 自报码优先于 internal 兜底（Error() 文案原样进 Message，不改写）。
	var coded Coded
	if errors.As(err, &coded) && coded.FaultCode() != "" {
		return Fail(coded.FaultCode(), err.Error())
	}
	return Fail("internal", err.Error())
}

// Reply 构造响应。err != nil 时保留调用方传入的 Result——错误与部分
// 结果并存（如等待超时/容量取消的 exec 响应仍携带日志地址；§2.4/§2.5）。
func Reply(protocol, id string, v any, err error) Response {
	r := Response{Protocol: protocol, ID: id, Result: v}
	if err != nil {
		r.Error = AsFault(err)
	}
	return r
}

var nameRe = regexp.MustCompile(`^[a-z][a-zA-Z0-9_.-]{0,63}$`)
var idRe = regexp.MustCompile(`^[A-Za-z0-9_:-]{1,128}$`)

func ValidName(v string) bool { return nameRe.MatchString(v) }
func ValidID(v string) bool   { return idRe.MatchString(v) }
func NewID(prefix string) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b)
}
func Decode(raw []byte, v any) error {
	if len(raw) == 0 || len(raw) > MaxMessageBytes || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return Fail("invalid_argument", "Expected bounded JSON object")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	d.UseNumber()
	if err := d.Decode(v); err != nil {
		return Fail("invalid_argument", err.Error())
	}
	if d.Decode(new(any)) != io.EOF {
		return Fail("invalid_argument", "Unexpected trailing JSON")
	}
	return nil
}

// Validate 校验请求身份与 action/载荷互斥（§4.1：每个 action 只接受对应载荷）。
func (r Request) Validate() error {
	if !ValidID(r.ID) || r.TimeoutMS < 0 || r.TimeoutMS > 1800000 {
		return Fail("invalid_argument", "Invalid request identity or timeout")
	}
	if r.Exec != nil && r.Action != ActionExec {
		return Fail("invalid_argument", "exec payload requires action=exec")
	}
	if r.FS != nil && r.Action != ActionFS {
		return Fail("invalid_argument", "fs payload requires action=fs")
	}
	if r.CancelID != "" && r.Action != ActionCancel {
		return Fail("invalid_argument", "cancel_id requires action=cancel")
	}
	switch r.Action {
	case ActionExec:
		return r.Exec.Validate()
	case ActionFS:
		if r.FS == nil || !ValidName(r.FS.Method) {
			return Fail("invalid_argument", "Invalid fs method")
		}
		var a map[string]any
		if err := Decode(r.FS.Args, &a); err != nil {
			return err
		}
		if a == nil {
			return Fail("invalid_argument", "args must be an object")
		}
	case ActionCancel:
		if !ValidID(r.CancelID) {
			return Fail("invalid_argument", "Invalid cancellation identity")
		}
	default:
		return Fail("unsupported", "Unknown transport action")
	}
	return nil
}

func (p *ExecPayload) Validate() error {
	if p == nil || p.Script == "" || len(p.Script) > MaxMessageBytes/2 || p.WaitMS < 0 || len(p.Workdir) > 4096 || len(p.Stdin) > MaxMessageBytes/2 {
		return Fail("invalid_argument", "Invalid exec arguments")
	}
	return nil
}
