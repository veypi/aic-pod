// Package hosts_tool binds tools once for all transports. Business state stays in handlers.
package hosts_tool

import (
	"context"
	"encoding/json"
	"fmt"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
	"io"
	"reflect"
	"strings"
	"time"
)

type Caller struct {
	Output       io.Writer // Per-execution output; never process-global stdout.
	Scope        string    // Empty = device capabilities; fs = owner file proxy.
	RequestID    string
	AllowStreams bool // Set only by the authenticated RTC adapter.
	Subject      string
	ConnectionID string
	Origin       string
	Level        int
	ExpiresAt    time.Time
	// Check is supplied by the authenticated adapter, never decoded from tool args.
	Expiry func() time.Time
	Check  func(context.Context) error
}

func (c Caller) Validate(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.Subject == "" || c.ConnectionID == "" || c.Level < 1 || !c.Deadline().After(time.Now()) {
		return wire.Fail("unauthorized", "Caller authorization expired")
	}
	if c.Check != nil {
		return c.Check(ctx)
	}
	return nil
}

func (c Caller) Deadline() time.Time {
	if c.Expiry != nil {
		return c.Expiry()
	}
	return c.ExpiresAt
}

// Stream is an opaque, duplex message endpoint. The RTC adapter pumps bytes in
// both directions; tool code owns all message framing and application semantics.
// Send and Recv run independently and concurrently. Implementations must honor
// cancellation, and Close must unblock both. Send acceptance is not a remote ACK.
type Stream interface {
	Recv(context.Context) ([]byte, error)
	Send(context.Context, []byte) error
	Close() error
}
type Spec struct {
	Background             bool
	AccessRules            []wire.AccessRule
	Name, Description, CLI string
	Access                 int
	Positionals            []string
}
type Method struct {
	Descriptor  wire.Method
	Positionals []string
	Validate    func(json.RawMessage) error
	Required    func(json.RawMessage) int
	Run         func(context.Context, Caller, json.RawMessage) (any, error)
	Open        func(context.Context, Caller, json.RawMessage) (Stream, error)
}
type Command struct {
	Desc, Help string
	Access     int
	RawArgv    bool
	Name       string
	Methods    []Method
	Close      func() error
}

func Bind[A, R any](spec Spec, fn func(context.Context, Caller, A) (R, error)) Method {
	return Method{Validate: func(raw json.RawMessage) error { var args A; return decode(raw, &args) }, Descriptor: descriptor[A, R](spec, wire.Call), Positionals: spec.Positionals, Run: func(ctx context.Context, c Caller, raw json.RawMessage) (any, error) {
		var args A
		if err := decode(raw, &args); err != nil {
			return nil, err
		}
		return fn(ctx, c, args)
	}}
}
func BindStream[A any](spec Spec, fn func(context.Context, Caller, A) (Stream, error)) Method {
	return Method{Descriptor: descriptor[A, struct{}](spec, wire.Stream), Positionals: spec.Positionals, Open: func(ctx context.Context, c Caller, raw json.RawMessage) (Stream, error) {
		var args A
		if err := decode(raw, &args); err != nil {
			return nil, err
		}
		return fn(ctx, c, args)
	}}
}
func DefineCommand(name string, methods ...Method) Command {
	return Command{Name: name, Methods: methods}
}
func descriptor[A, R any](s Spec, mode wire.Mode) wire.Method {
	in, _ := json.Marshal(schema(reflect.TypeFor[A]()))
	out, _ := json.Marshal(schema(reflect.TypeFor[R]()))
	if mode == wire.Stream {
		out = nil
	}
	return wire.Method{Background: s.Background, AccessRules: s.AccessRules, Name: s.Name, Mode: mode, Access: s.Access, Input: in, Output: out, Description: s.Description, CLI: s.CLI, Positionals: s.Positionals}
}
func schema(t reflect.Type) map[string]any {
	if t == reflect.TypeFor[json.RawMessage]() {
		return map[string]any{}
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		props := map[string]any{}
		required := []string{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" && f.Anonymous {
				nested := schema(f.Type)
				if children, ok := nested["properties"].(map[string]any); ok {
					for k, v := range children {
						props[k] = v
					}
					if req, ok := nested["required"].([]string); ok {
						required = append(required, req...)
					}
					continue
				}
			}
			if name == "" {
				name = f.Name
			}
			v := schema(f.Type)
			if e := f.Tag.Get("enum"); e != "" {
				v["enum"] = strings.Split(e, ",")
			}
			props[name] = v
			if f.Tag.Get("required") == "true" {
				required = append(required, name)
			}
		}
		return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
	case reflect.Map:
		return map[string]any{"type": "object"}
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return map[string]any{"type": "string", "contentEncoding": "base64"}
		}
		return map[string]any{"type": "array", "items": schema(t.Elem())}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.String:
		return map[string]any{"type": "string"}
	default:
		return map[string]any{}
	}
}
func decode(raw []byte, args any) error {
	if err := wire.Decode(raw, args); err != nil {
		return err
	}
	// Required fields must exist, including false and zero where meaningful.
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return wire.Fail("invalid_argument", "args must be an object")
	}
	t := reflect.TypeOf(args).Elem()
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() == reflect.Struct {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if f.Tag.Get("required") == "true" && (len(obj[name]) == 0 || string(obj[name]) == "null") {
				return wire.Fail("invalid_argument", "Missing "+name)
			}
			if choices := f.Tag.Get("enum"); choices != "" && len(obj[name]) > 0 {
				var v string
				if json.Unmarshal(obj[name], &v) != nil {
					return wire.Fail("invalid_argument", "Invalid "+name)
				}
				found := false
				for _, x := range strings.Split(choices, ",") {
					found = found || v == x
				}
				if !found {
					return wire.Fail("invalid_argument", fmt.Sprintf("%s must be one of %s", name, choices))
				}
			}
		}
	}
	if v, ok := args.(interface{ Validate() error }); ok {
		return v.Validate()
	}
	return nil
}
