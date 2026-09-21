package hosts_tool

import (
	"encoding/json"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
	"strings"
)

// Translate uses registered field schemas, never a shell or a tool-specific parser.
func (d *Dispatcher) Translate(argv []string) (wire.Invocation, error) {
	fail := func() (wire.Invocation, error) {
		return wire.Invocation{}, wire.Fail("invalid_argument", "Expected tool method [--field value]; nested fields use JSON values")
	}
	if len(argv) < 1 {
		return fail()
	}
	d.mu.Lock()
	t, ok := d.commands[argv[0]]
	d.mu.Unlock()
	if !ok {
		return fail()
	}
	descriptor := wire.Command{Name: t.Name, RawArgv: t.RawArgv}
	for _, method := range t.Methods {
		descriptor.Methods = append(descriptor.Methods, method.Descriptor)
	}
	return Translate(descriptor, argv[1:])
}

// Translate is pure metadata translation, also used by callers to evaluate declared access rules.
func Translate(t wire.Command, argv []string) (wire.Invocation, error) {
	fail := func() (wire.Invocation, error) {
		return wire.Invocation{}, wire.Fail("invalid_argument", "Expected method [--field value]; nested values use JSON")
	}
	if t.RawArgv {
		raw, _ := json.Marshal(map[string]any{"argv": argv})
		return wire.Invocation{Domain: "exec", Command: t.Name, Method: "run", Args: raw}, nil
	}
	if len(argv) == 0 {
		return fail()
	}
	var method *wire.Method
	for _, m := range t.Methods {
		if m.Mode == wire.Call && (m.Name == argv[0] || m.CLI != "" && m.CLI == argv[0]) {
			copy := m
			method = &copy
			break
		}
	}
	if method == nil {
		return fail()
	}
	var spec struct {
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	_ = json.Unmarshal(method.Input, &spec)
	args := map[string]any{}
	pos := 0
	for i := 1; i < len(argv); i++ {
		token := argv[i]
		field, value := "", token
		if strings.HasPrefix(token, "--") {
			field = strings.TrimPrefix(token, "--")
			if at := strings.IndexByte(field, '='); at >= 0 {
				value = field[at+1:]
				field = strings.ReplaceAll(field[:at], "-", "_")
			} else {
				field = strings.ReplaceAll(field, "-", "_")
				v, exists := spec.Properties[field]
				if !exists {
					return fail()
				}
				if v.Type == "boolean" && (i+1 == len(argv) || strings.HasPrefix(argv[i+1], "--")) {
					value = "true"
				} else {
					i++
					if i == len(argv) {
						return fail()
					}
					value = argv[i]
				}
			}
		} else {
			if pos >= len(method.Positionals) {
				return fail()
			}
			field = method.Positionals[pos]
			pos++
		}
		prop, exists := spec.Properties[field]
		if !exists {
			return fail()
		}
		if _, dup := args[field]; dup {
			return fail()
		}
		if prop.Type == "string" {
			args[field] = value
		} else {
			var v any
			if json.Unmarshal([]byte(value), &v) != nil {
				return fail()
			}
			args[field] = v
		}
	}
	raw, _ := json.Marshal(args)
	return wire.Invocation{Domain: "exec", Command: t.Name, Method: method.Name, Args: raw}, nil
}
