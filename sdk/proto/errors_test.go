package proto

import (
	"fmt"
	"testing"
)

func TestErrorModel(t *testing.T) {
	e := &ExecError{Tool: "fs", Action: "read", Reason: "offset 100 exceeds 42 lines"}
	if e.Error() != "fs read: offset 100 exceeds 42 lines" {
		t.Errorf("ExecError = %q", e.Error())
	}
	e2 := &ExecError{Tool: "fs", Reason: "path outside allowed roots: /etc"}
	if e2.Error() != "fs: path outside allowed roots: /etc" {
		t.Errorf("ExecError(no action) = %q", e2.Error())
	}

	if s := StateOf(&DeniedError{Reason: "x"}); s != StateRejected {
		t.Errorf("StateOf(Denied) = %q", s)
	}
	if s := StateOf(&ApprovalError{Reason: "x"}); s != StateWaiting {
		t.Errorf("StateOf(Approval) = %q", s)
	}
	if s := StateOf(fmt.Errorf("boom")); s != StateError {
		t.Errorf("StateOf(generic) = %q", s)
	}
	// 包装后仍可识别
	if s := StateOf(fmt.Errorf("wrap: %w", &DeniedError{Reason: "x"})); s != StateRejected {
		t.Errorf("StateOf(wrapped Denied) = %q", s)
	}
}
