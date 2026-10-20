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
	for _, condition := range []FSCondition{{}, {Absent: true, Any: true}, {Version: "v1", Any: true}} {
		if err := condition.Check("v1", true); err == nil {
			t.Fatal("ambiguous write condition accepted")
		}
	}
	if err := (FSCondition{Absent: true}).Check("v1", true); err == nil {
		t.Fatal("create overwrote existing file")
	}
	if err := (FSCondition{Version: "v1"}).Check("v2", true); err == nil {
		t.Fatal("stale version accepted")
	}
	if err := (FSCondition{Version: "v1"}).Check("v1", true); err != nil {
		t.Fatal(err)
	}
	if err := (FSCondition{Any: true}).Check("v2", true); err != nil {
		t.Fatal(err)
	}
}
