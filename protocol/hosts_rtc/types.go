// Package hosts_rtc defines authenticated calls and opaque RTC tool channels.
package hosts_rtc

import wire "github.com/veypi/aic-pod/protocol/hosts_tools"

const Protocol = "hosts_rtc/2"
const Channel = "hosts-tools"
const StreamPrefix = "hosts-stream/"
const MaxDatagramBytes = 32 << 10

// Request 复用 hosts_tools 载荷；Ticket 服务于 hello/auth.renew；
// GrantApproved 是 owner 前端对本次请求的确认元信息（默认不批准；
// agent 不使用本入口——AI 调用走服务端审批路径）。
type Request struct {
	wire.Request
	Channel       string `json:"channel,omitempty"`
	Ticket        string `json:"ticket,omitempty"`
	GrantApproved bool   `json:"grant_approved,omitempty"`
	// Stream 是 action=stream.open 的载荷（RTC 包限定端点，不进 vsh/caps）。
	Stream *StreamOpen `json:"stream,omitempty"`
}

// StreamOpen 是 stream.open 的载荷：browser.page.frames / browser.page.input 等 RTC 私有
// 端点直接连接业务服务，不注册为 vsh 指令，不进入 commands/caps。
type StreamOpen struct {
	Endpoint string `json:"endpoint"`
	Args     any    `json:"args,omitempty"`
}

type Closed struct {
	Protocol string      `json:"protocol"`
	Event    string      `json:"event"`
	Channel  string      `json:"channel"`
	Error    *wire.Fault `json:"error,omitempty"`
}

type Fault = wire.Fault

var ValidID = wire.ValidID
