package hosts

import (
	"encoding/binary"
	"encoding/json"
)

const MaxDataBytes = 32 << 10
const ChunkBytes = 16 << 10
const MaxHeaderBytes = 1024

type Frame struct {
	RequestID string          `json:"request_id,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	StreamID  string          `json:"stream_id"`
	ItemID    string          `json:"item_id"`
	Seq       int64           `json:"seq"`
	Offset    int64           `json:"offset"`
	Final     bool            `json:"final"`
	Size      int64           `json:"size"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
}

func (f Frame) Validate() error {
	if !ValidID(f.StreamID) || !ValidID(f.ItemID) || f.Seq < 0 || f.Seq > MaxSafeInteger || f.Offset < 0 || f.Offset > MaxSafeInteger {
		return Fail("invalid_argument", "Invalid binary frame header")
	}
	return nil
}
func EncodeFrame(f Frame, payload []byte) ([]byte, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	h, err := json.Marshal(f)
	if err != nil {
		return nil, err
	}
	if len(h) > MaxHeaderBytes || 4+len(h)+len(payload) > MaxDataBytes {
		return nil, Fail("invalid_argument", "Binary frame exceeds limit")
	}
	out := make([]byte, 4+len(h)+len(payload))
	binary.BigEndian.PutUint32(out, uint32(len(h)))
	copy(out[4:], h)
	copy(out[4+len(h):], payload)
	return out, nil
}
func DecodeFrame(raw []byte) (Frame, []byte, error) {
	var f Frame
	if len(raw) < 4 || len(raw) > MaxDataBytes {
		return f, nil, Fail("invalid_argument", "Invalid binary frame size")
	}
	n := int(binary.BigEndian.Uint32(raw))
	if n < 2 || n > MaxHeaderBytes || n > len(raw)-4 {
		return f, nil, Fail("invalid_argument", "Invalid binary header size")
	}
	if err := Decode(raw[4:4+n], &f); err != nil {
		return f, nil, err
	}
	if err := f.Validate(); err != nil {
		return f, nil, err
	}
	return f, raw[4+n:], nil
}

type StreamItem struct {
	StreamID  string `json:"stream_id"`
	ItemID    string `json:"item_id"`
	Size      int64  `json:"size"`
	MediaType string `json:"media_type"`
}
type Event struct {
	V     int             `json:"v"`
	Type  string          `json:"type"`
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
}
