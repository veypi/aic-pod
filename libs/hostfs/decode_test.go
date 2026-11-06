//go:build darwin || linux || windows

package hostfs

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/veypi/aic-pod/protocol"
)

func TestRunRejectsMalformedArgumentsBeforeAuthorization(t *testing.T) {
	f := setup(t)
	checks := 0
	f.fs.cfg.Check = func(context.Context, Call, string, bool) error {
		checks++
		return nil
	}
	for _, method := range []string{"roots", "home", "os_home", "stat", "read", "list", "write", "find", "move", "copy", "mkdir", "remove"} {
		t.Run(method, func(t *testing.T) {
			checks = 0
			_, err := f.fs.Run(context.Background(), Call{Caller: f.caller, Owner: f.owner, Command: "fs", Method: method, Args: json.RawMessage(`{"path":`)})
			if fault := protocol.AsFault(err); fault == nil || fault.Code != "invalid_argument" {
				t.Fatalf("malformed args error = %+v, want invalid_argument", fault)
			}
			if checks != 0 {
				t.Fatalf("policy consulted %d times before valid arguments", checks)
			}
		})
	}
}

func TestRunArgumentValidationErrorPrecedence(t *testing.T) {
	f := setup(t)
	for _, tc := range []struct {
		name, command, method, raw, want string
	}{
		{name: "unknown method", command: "fs", method: "unknown", raw: `{}`, want: "unsupported"},
		{name: "command before malformed args", command: "other", method: "stat", raw: `{"path":`, want: "unsupported"},
		{name: "trailing JSON", command: "fs", method: "stat", raw: `{} {}`, want: "invalid_argument"},
		{name: "unknown field", command: "fs", method: "stat", raw: `{"path":{"root_id":"home","segments":[]},"unknown":true}`, want: "invalid_argument"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.fs.Run(context.Background(), Call{Caller: f.caller, Owner: f.owner, Command: tc.command, Method: tc.method, Args: json.RawMessage(tc.raw)})
			if fault := protocol.AsFault(err); fault == nil || fault.Code != tc.want {
				t.Fatalf("error = %+v, want %s", fault, tc.want)
			}
		})
	}
}

type authorizationRecord struct {
	base  string
	write bool
}

func TestParsedArgsUseExpectedAuthorizationPathsAndWriteModes(t *testing.T) {
	f := setup(t)
	var records []authorizationRecord
	f.fs.cfg.Check = func(_ context.Context, _ Call, path string, write bool) error {
		records = append(records, authorizationRecord{base: filepath.Base(path), write: write})
		return nil
	}
	for _, tc := range []struct {
		name, method string
		args         any
		want         []authorizationRecord
	}{
		{name: "stat", method: "stat", args: pathArgs{Path: loc("item")}, want: []authorizationRecord{{base: "item", write: false}}},
		{name: "list", method: "list", args: listArgs{Path: loc("item")}, want: []authorizationRecord{{base: "item", write: false}}},
		{name: "find", method: "find", args: findArgs{Path: loc("item")}, want: []authorizationRecord{{base: "item", write: false}}},
		{name: "remove", method: "remove", args: removeArgs{Path: loc("item")}, want: []authorizationRecord{{base: "item", write: true}}},
		{name: "move source and target", method: "move", args: moveArgs{Src: loc("src"), Dst: loc("dst"), Condition: protocol.FSCondition{Absent: true}}, want: []authorizationRecord{{base: "src", write: true}, {base: "dst", write: true}}},
		{name: "copy source and target", method: "copy", args: moveArgs{Src: loc("src"), Dst: loc("dst"), Condition: protocol.FSCondition{Absent: true}}, want: []authorizationRecord{{base: "src", write: false}, {base: "dst", write: true}}},
		{name: "mkdir", method: "mkdir", args: mkdirArgs{Path: loc("item")}, want: []authorizationRecord{{base: "item", write: true}}},
		{name: "mkdir parents defers to created levels", method: "mkdir", args: mkdirArgs{Path: loc("parent", "item"), Parents: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			records = nil
			raw, err := json.Marshal(tc.args)
			if err != nil {
				t.Fatal(err)
			}
			params, err := parseArgs(tc.method, raw)
			if err == nil {
				err = validateArgs(tc.method, params)
			}
			if err == nil {
				call := Call{Caller: f.caller, Owner: f.owner, Command: "fs", Method: tc.method}
				err = f.fs.authorizeParsed(context.Background(), call, params)
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(records, tc.want) {
				t.Fatalf("authorization = %+v, want %+v", records, tc.want)
			}
		})
	}
}

func TestExecuteParsedValidatesTypedArgumentsBeforeAuthorization(t *testing.T) {
	f := setup(t)
	checks := 0
	f.fs.cfg.Check = func(context.Context, Call, string, bool) error {
		checks++
		return nil
	}
	for _, tc := range []struct {
		method string
		params any
	}{
		{method: "write", params: writeArgs{Path: loc("item")}},
		{method: "mkdir", params: mkdirArgs{}},
		{method: "remove", params: removeArgs{}},
		{method: "move", params: moveArgs{Src: loc("src"), Dst: loc("dst")}},
	} {
		t.Run(tc.method, func(t *testing.T) {
			checks = 0
			call := Call{Caller: f.caller, Owner: f.owner, Command: "fs", Method: tc.method}
			_, err := f.fs.executeParsed(context.Background(), call, tc.params)
			if fault := protocol.AsFault(err); fault == nil || fault.Code != "invalid_argument" {
				t.Fatalf("typed arguments error = %+v, want invalid_argument", fault)
			}
			if checks != 0 {
				t.Fatalf("policy consulted %d times before valid typed arguments", checks)
			}
		})
	}
}
