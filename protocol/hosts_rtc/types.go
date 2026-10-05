// Package hosts_rtc defines the authenticated device data channel envelope.
package hosts_rtc

import (
	wire "github.com/veypi/aic-pod/protocol/tool"
)

const Protocol = "hosts_rtc/3"
const Channel = "aic-tools"
const BrowserChannel = "aic-browser"
const BrowserMessageLimit = 16 << 20

// Exactly one authentication or platform request per frame.
type Request struct {
	Tool          *wire.Request `json:"tool,omitempty"`
	ID            string        `json:"id,omitempty"`
	Auth          string        `json:"auth,omitempty"`
	Ticket        string        `json:"ticket,omitempty"`
	GrantApproved bool          `json:"grant_approved,omitempty"`
}
type AuthResult struct {
	ID     string      `json:"id"`
	Result any         `json:"result,omitempty"`
	Error  *wire.Fault `json:"error,omitempty"`
}
type Fault = wire.Fault

var ValidID = wire.ValidID

func (r Request) Validate() error {
	if r.GrantApproved && (r.Tool == nil || r.Tool.Action != wire.ActionExec) {
		return wire.Fail("invalid_argument", "Approval belongs to an exec request")
	}
	count := 0
	if r.Auth != "" {
		count++
		if !wire.ValidID(r.ID) || r.Ticket == "" || r.GrantApproved {
			return wire.Fail("invalid_argument", "Invalid authentication frame")
		}
	}
	if r.Tool != nil {
		count++
		if r.Tool.Protocol != Protocol {
			return wire.Fail("unsupported", "Invalid device protocol")
		}
		if err := r.Tool.Validate(); err != nil {
			return err
		}
	}
	if count != 1 || r.Auth == "" && (r.ID != "" || r.Ticket != "") {
		return wire.Fail("invalid_argument", "Expected exactly one frame payload")
	}
	return nil
}
