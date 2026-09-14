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

// StrategyError：策略类错误提取（保型上抛的前置判定）——包装链可识别、
// 非策略错误返回 nil。
func TestStrategyError(t *testing.T) {
	if se := StrategyError(fmt.Errorf("boom")); se != nil {
		t.Errorf("StrategyError(generic) = %v, want nil", se)
	}
	ae := &ApprovalError{Reason: "fs: write requires fs level 3"}
	if se := StrategyError(fmt.Errorf("wrap: %w", ae)); se != ae {
		t.Errorf("StrategyError(wrapped Approval) = %v, want %v", se, ae)
	}
	de := &DeniedError{Reason: "deny"}
	if se := StrategyError(de); se != de {
		t.Errorf("StrategyError(Denied) = %v, want %v", se, de)
	}
	// 多层包装：返回最深的策略错误
	if se := StrategyError(fmt.Errorf("l0: %w", fmt.Errorf("l1: %w", ae))); se != ae {
		t.Errorf("StrategyError(nested) = %v, want %v", se, ae)
	}
}
