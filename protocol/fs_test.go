package protocol

import (
	"errors"
	"io/fs"
	"testing"
)

func TestStructuredPathValidation(t *testing.T) {
	for _, path := range []FSPath{{RootID: "home", Segments: []string{}}, {RootID: "home", Segments: []string{"中文 空格", "100%#?.txt"}}} {
		if err := path.Validate(false); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []FSPath{{RootID: "home"}, {RootID: "", Segments: []string{}}, {RootID: "home", Segments: []string{".."}}, {RootID: "home", Segments: []string{"."}}, {RootID: "home", Segments: []string{""}}, {RootID: "home", Segments: []string{"a/b"}}, {RootID: "home", Segments: []string{"a\x00b"}}} {
		if err := path.Validate(false); err == nil {
			t.Fatalf("invalid path accepted: %#v", path)
		}
	}
	for _, name := range []string{`C:`, `a\b`, "file:stream", "trailing.", "trailing "} {
		if err := (FSPath{RootID: "drive", Segments: []string{name}}).Validate(true); err == nil {
			t.Fatalf("Windows segment accepted: %q", name)
		}
	}
}

func TestFSInvocationSurface(t *testing.T) {
	args := []byte(`{}`)
	for _, req := range []Request{
		{ID: "r_1", Action: ActionFS, FS: &FSInvocation{Method: "text", Action: "read", Args: args}},
		{ID: "r_2", Action: ActionFS, FS: &FSInvocation{Method: "stat", Args: args}},
	} {
		if err := req.Validate(); err != nil {
			t.Fatalf("valid fs invocation rejected: %+v: %v", req.FS, err)
		}
	}
	for _, req := range []Request{
		{ID: "r_3", Action: ActionFS, FS: &FSInvocation{Method: "text", Args: args}},
		{ID: "r_4", Action: ActionFS, FS: &FSInvocation{Method: "stat", Action: "read", Args: args}},
	} {
		if err := req.Validate(); err == nil {
			t.Fatalf("invalid fs invocation accepted: %+v", req.FS)
		}
	}
}

func TestFSFaultClassification(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code string
	}{
		{fs.ErrNotExist, "not_found"},
		{fs.ErrExist, "already_exists"},
		{fs.ErrPermission, "permission_denied"},
	} {
		fault, ok := FSFault(tc.err)
		if !ok || fault.Code != tc.code {
			t.Fatalf("FSFault(%v) = %+v, %v; want %s", tc.err, fault, ok, tc.code)
		}
	}
	if fault, ok := FSFault(errors.New("boom")); ok || fault != nil {
		t.Fatalf("unrecognized error must not be classified: %+v, %v", fault, ok)
	}
	if fault, ok := FSFault(FSFail("cancelled", "stop")); !ok || fault.Code != "cancelled" {
		t.Fatalf("existing fault must pass through: %+v, %v", fault, ok)
	}
}

func TestWriteConditionsRequireExplicitIntent(t *testing.T) {
	const hash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	for _, condition := range []FSCondition{{}, {Absent: true, Any: true}, {Hash: hash, Any: true}} {
		if err := condition.Check(true); err == nil {
			t.Fatal("ambiguous write condition accepted")
		}
	}
	if err := (FSCondition{Absent: true}).Check(true); err == nil {
		t.Fatal("create overwrote existing file")
	}
	if err := (FSCondition{Any: true}).Check(true); err != nil {
		t.Fatal(err)
	}
	if err := (FSCondition{Hash: hash}).Check(true, hash); err != nil {
		t.Fatal(err)
	}
	if err := (FSCondition{Hash: hash}).Check(false, ""); protocolCode(err) != "file_hash_conflict" {
		t.Fatalf("deleted file hash conflict = %v", err)
	}
	if err := (FSCondition{Hash: hash}).Check(true); protocolCode(err) != "unsupported" {
		t.Fatalf("non-write hash condition = %v", err)
	}
	if err := (FSCondition{Hash: "bad"}).Check(true, "bad"); protocolCode(err) != "invalid_file_hash" {
		t.Fatalf("invalid content hash = %v", err)
	}
}

func protocolCode(err error) string {
	if err == nil {
		return ""
	}
	return AsFault(err).Code
}
