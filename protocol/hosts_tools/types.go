// Package hosts_tools defines the transport-independent tool contract.
package hosts_tools

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

const Protocol = "hosts_tools/1"
const MaxMessageBytes = 1 << 20

type Mode string

const (
	Call   Mode = "call"
	Stream Mode = "stream"
)

type AccessRule struct {
	Field  string `json:"field"`
	Equals string `json:"equals"`
	Level  int    `json:"level"`
}
type Method struct {
	Background  bool            `json:"background,omitempty"`
	AccessRules []AccessRule    `json:"access_rules,omitempty"`
	Name        string          `json:"name"`
	Mode        Mode            `json:"mode"`
	Description string          `json:"description,omitempty"`
	Input       json.RawMessage `json:"input,omitempty"`
	Output      json.RawMessage `json:"output,omitempty"`
	Access      int             `json:"access"`
	CLI         string          `json:"cli,omitempty"`
	Positionals []string        `json:"positionals,omitempty"`
}
type Command struct {
	Desc          string   `json:"desc,omitempty"`
	Help          string   `json:"help,omitempty"`
	RequiredLevel int      `json:"level"`
	RawArgv       bool     `json:"raw_argv,omitempty"`
	Name          string   `json:"name"`
	Methods       []Method `json:"methods"`
}
type Catalog struct {
	FS   []Method `json:"fs"`
	Exec struct {
		Epoch    string    `json:"epoch,omitempty"`
		Commands []Command `json:"commands"`
	} `json:"exec"`
}

// CatalogQuery requests one complete capability declaration; omitted means a lightweight index.
type CatalogQuery struct {
	Domain  string `json:"domain"`
	Command string `json:"command,omitempty"`
}

type Invocation struct {
	Domain  string          `json:"domain"`
	Command string          `json:"command,omitempty"`
	Method  string          `json:"method"`
	Args    json.RawMessage `json:"args"`
}

// Request has no business session, operation or resource identity.
type Request struct {
	Catalog   *CatalogQuery     `json:"catalog,omitempty"`
	Protocol  string            `json:"protocol"`
	ID        string            `json:"request_id"`
	Action    string            `json:"action"`
	Call      *Invocation       `json:"call,omitempty"`
	Argv      []string          `json:"argv,omitempty"`
	Execution *ExecutionOptions `json:"execution,omitempty"`
	TimeoutMS int64             `json:"timeout_ms,omitempty"`
	CancelID  string            `json:"cancel_id,omitempty"`
	Ticket    string            `json:"ticket,omitempty"`
}
type ExecutionOptions struct {
	Epoch  string `json:"epoch"`
	ID     string `json:"id"`
	WaitMS *int64 `json:"wait_ms,omitempty"`
	Output string `json:"output,omitempty"`
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
	return Fail("internal", err.Error())
}
func Reply(protocol, id string, v any, err error) Response {
	r := Response{Protocol: protocol, ID: id, Result: v}
	if err != nil {
		r.Result = nil
		r.Error = AsFault(err)
	}
	return r
}

var name = regexp.MustCompile(`^[a-z][a-zA-Z0-9_.-]{0,63}$`)
var id = regexp.MustCompile(`^[A-Za-z0-9_:-]{1,128}$`)

func ValidName(v string) bool { return name.MatchString(v) }
func ValidID(v string) bool   { return id.MatchString(v) }
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
func (r Request) Validate() error {
	if !ValidID(r.ID) || r.TimeoutMS < 0 || r.TimeoutMS > 1800000 {
		return Fail("invalid_argument", "Invalid request identity or timeout")
	}
	if r.Catalog != nil && (r.Action != "catalog" || (r.Catalog.Domain != "fs" && r.Catalog.Domain != "exec") || (r.Catalog.Domain == "exec" && !ValidName(r.Catalog.Command)) || (r.Catalog.Domain == "fs" && r.Catalog.Command != "")) {
		return Fail("invalid_argument", "Invalid catalog selector")
	}
	if r.Execution != nil {
		if r.Action != "call" || !ValidID(r.Execution.ID) || !ValidID(r.Execution.Epoch) || (r.Execution.WaitMS != nil && (*r.Execution.WaitMS < 0 || *r.Execution.WaitMS > 300000)) || (r.Call != nil && r.Call.Domain != "exec") {
			return Fail("invalid_argument", "Invalid exec execution options")
		}
	}
	switch r.Action {
	case "catalog":
	case "call":
		if (r.Call == nil) == (len(r.Argv) == 0) {
			return Fail("invalid_argument", "Provide exactly one of call or argv")
		}
		if r.Call != nil {
			if (r.Call.Domain != "fs" && r.Call.Domain != "exec") || (r.Call.Domain == "exec" && !ValidName(r.Call.Command)) || (r.Call.Domain == "fs" && r.Call.Command != "") || !ValidName(r.Call.Method) {
				return Fail("invalid_argument", "Invalid tool or method")
			}
			var a map[string]any
			if err := Decode(r.Call.Args, &a); err != nil {
				return err
			}
			if a == nil {
				return Fail("invalid_argument", "args must be an object")
			}
		}
	case "call.cancel":
		if !ValidID(r.CancelID) {
			return Fail("invalid_argument", "Invalid cancellation identity")
		}
	default:
		return Fail("unsupported", "Unknown transport action")
	}
	return nil
}

// RequiredLevel allows argument-dependent grants to be declared once by a tool.
func (m Method) RequiredLevel(args json.RawMessage) int {
	level := m.Access
	var values map[string]json.RawMessage
	if json.Unmarshal(args, &values) != nil {
		return level
	}
	for _, rule := range m.AccessRules {
		var value string
		if json.Unmarshal(values[rule.Field], &value) == nil && value == rule.Equals && rule.Level > level {
			level = rule.Level
		}
	}
	return level
}

func (in Invocation) Target() string {
	if in.Domain == "fs" {
		return "fs"
	}
	return in.Command
}
