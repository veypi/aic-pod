// Package hosts_rtc defines authenticated calls and opaque RTC tool channels.
package hosts_rtc

import wire "github.com/veypi/aic-pod/protocol/hosts_tools"

const Protocol = "hosts_rtc/1"
const Channel = "hosts-tools"
const StreamPrefix = "hosts-stream/"
const MaxDatagramBytes = 32 << 10

type Request struct {
	wire.Request
	Channel string `json:"channel,omitempty"`
}
type Closed struct {
	Protocol string      `json:"protocol"`
	Event    string      `json:"event"`
	Channel  string      `json:"channel"`
	Error    *wire.Fault `json:"error,omitempty"`
}

type Fault = wire.Fault

var ValidID = wire.ValidID
