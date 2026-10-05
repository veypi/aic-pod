// hosts_rtc/3 已认证设备数据通道信封。
package protocol

const RtcProtocol = "hosts_rtc/3"
const RtcChannel = "aic-tools"
const RtcBrowserChannel = "aic-browser"
const RtcBrowserMessageLimit = 16 << 20

// Tool responses larger than one chunk use binary frames: uint32 BE payload
// length, then ordered chunks of the original JSON. Requests remain JSON text.
const RtcToolResponseLimit = 16 << 20
const RtcToolResponseChunkSize = 16 << 10

// Exactly one authentication or platform request per frame.
type RtcRequest struct {
	Tool          *Request `json:"tool,omitempty"`
	ID            string   `json:"id,omitempty"`
	Auth          string   `json:"auth,omitempty"`
	Ticket        string   `json:"ticket,omitempty"`
	GrantApproved bool     `json:"grant_approved,omitempty"`
}
type RtcAuthResult struct {
	ID     string `json:"id"`
	Result any    `json:"result,omitempty"`
	Error  *Fault `json:"error,omitempty"`
}

func (r RtcRequest) Validate() error {
	if r.GrantApproved && (r.Tool == nil || r.Tool.Action != ActionExec) {
		return Fail("invalid_argument", "Approval belongs to an exec request")
	}
	count := 0
	if r.Auth != "" {
		count++
		if !ValidID(r.ID) || r.Ticket == "" || r.GrantApproved {
			return Fail("invalid_argument", "Invalid authentication frame")
		}
	}
	if r.Tool != nil {
		count++
		if r.Tool.Protocol != RtcProtocol {
			return Fail("unsupported", "Invalid device protocol")
		}
		if err := r.Tool.Validate(); err != nil {
			return err
		}
	}
	if count != 1 || r.Auth == "" && (r.ID != "" || r.Ticket != "") {
		return Fail("invalid_argument", "Expected exactly one frame payload")
	}
	return nil
}
