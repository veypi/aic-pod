// Package hosts defines the machine-facing hosts/1 protocol. It deliberately
// contains no presentation formatting or RTC dependencies.
package hosts

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
)

const Protocol = "hosts/1"
const MaxControlBytes = 64 << 10
const MaxSafeInteger int64 = 1<<53 - 1

// Fault is the only machine-readable error representation. A failed transport
// request has no admitted operation; an admitted failure belongs to Operation.
type Fault struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
	Retry   string         `json:"retry"`
	Effect  string         `json:"effect"`
}

func (e *Fault) Error() string { return e.Code + ": " + e.Message }
func Fail(code, message string) *Fault {
	return &Fault{Code: code, Message: message, Retry: "never", Effect: "none"}
}
func AsFault(err error) *Fault {
	var f *Fault
	if errors.As(err, &f) {
		cp := *f
		return &cp
	}
	return Fail("internal", "Device operation failed")
}

var validID = regexp.MustCompile(`^[A-Za-z0-9_:-]{1,128}$`)
var validName = regexp.MustCompile(`^[a-z][a-zA-Z0-9_.-]{0,63}$`)

func ValidID(s string) bool   { return validID.MatchString(s) }
func ValidName(s string) bool { return validName.MatchString(s) }
func NewID(prefix string) (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// Decode is strict at protocol boundaries, including trailing JSON and null.
func Decode(raw []byte, dst any) error {
	if len(raw) == 0 || len(raw) > MaxControlBytes || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return Fail("invalid_argument", "Expected a bounded JSON object")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	d.UseNumber()
	if err := d.Decode(dst); err != nil {
		return Fail("invalid_argument", err.Error())
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return Fail("invalid_argument", "Unexpected trailing JSON")
	}
	return nil
}

// CanonicalArgs also ensures that arbitrary command parameters are an object.
func CanonicalArgs(raw json.RawMessage) (json.RawMessage, error) {
	var args map[string]any
	if err := Decode(raw, &args); err != nil {
		return nil, err
	}
	if args == nil {
		return nil, Fail("invalid_argument", "args must be an object")
	}
	b, err := json.Marshal(args)
	return b, err
}

type Request struct {
	V      int             `json:"v"`
	Type   string          `json:"type"`
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func ParseRequest(raw []byte) (Request, error) {
	var r Request
	if err := Decode(raw, &r); err != nil {
		return r, err
	}
	if r.V != 1 {
		return r, Fail("unsupported_protocol", "Expected hosts/1")
	}
	if r.Type != "request" || !ValidID(r.ID) || !ValidName(r.Method) {
		return r, Fail("invalid_argument", "Invalid request envelope")
	}
	if _, err := CanonicalArgs(r.Params); err != nil {
		return r, err
	}
	return r, nil
}

type Response struct {
	V      int    `json:"v"`
	Type   string `json:"type"`
	ID     string `json:"id"`
	OK     bool   `json:"ok"`
	Result any    `json:"result,omitempty"`
	Error  *Fault `json:"error,omitempty"`
}

func Reply(id string, value any, err error) Response {
	r := Response{V: 1, Type: "response", ID: id, OK: err == nil, Result: value}
	if err != nil {
		r.Result = nil
		r.Error = AsFault(err)
	}
	return r
}

type ResourceRef struct {
	ID    string `json:"id"`
	Epoch string `json:"epoch"`
	Kind  string `json:"kind"`
}
type Invocation struct {
	SessionID    string          `json:"session_id"`
	RuntimeEpoch string          `json:"runtime_epoch"`
	OperationID  string          `json:"operation_id"`
	Command      string          `json:"command"`
	Method       string          `json:"method"`
	Args         json.RawMessage `json:"args"`
	TimeoutMS    int64           `json:"timeout_ms,omitempty"`
}

func (r Invocation) Validate() error {
	if !ValidID(r.SessionID) || !ValidID(r.RuntimeEpoch) || !ValidID(r.OperationID) || !ValidName(r.Command) || !ValidName(r.Method) {
		return Fail("invalid_argument", "Invalid invocation identity")
	}
	if r.TimeoutMS < 0 || r.TimeoutMS > 300000 {
		return Fail("invalid_argument", "timeout_ms must be between 0 and 300000")
	}
	_, err := CanonicalArgs(r.Args)
	return err
}

type Method struct {
	// Request methods are transient: no operation ledger and no automatic replay.
	// Live methods open a connection-bound duplex stream with no replay.
	Mode         string          `json:"mode,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema"`
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
	Effect       string          `json:"effect"`
}
type Command struct {
	Name     string            `json:"name"`
	Contract string            `json:"contract"`
	Methods  map[string]Method `json:"methods"`
}

func (c Command) Validate() error {
	if !ValidName(c.Name) || c.Contract == "" || len(c.Methods) == 0 {
		return fmt.Errorf("command requires name, contract and methods")
	}
	for name, m := range c.Methods {
		if !ValidName(name) || (m.Effect != "read" && m.Effect != "write") || (m.Mode != "" && m.Mode != "request" && m.Mode != "live") {
			return fmt.Errorf("invalid method %q", name)
		}
		if _, err := CanonicalArgs(m.InputSchema); err != nil {
			return fmt.Errorf("%s schema: %w", name, err)
		}
	}
	return nil
}

type Operation struct {
	ID              string          `json:"operation_id"`
	Status          string          `json:"status"`
	Value           json.RawMessage `json:"value,omitempty"`
	Error           *Fault          `json:"error,omitempty"`
	CancelRequested bool            `json:"cancel_requested,omitempty"`
}

func (o Operation) Terminal() bool {
	return o.Status == "succeeded" || o.Status == "failed" || o.Status == "cancelled"
}
