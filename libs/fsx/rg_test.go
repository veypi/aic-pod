package fsx

// rg_test.go — rg 的 limit/context 越界钳制与失败归类（2026-10-10）。
// 语义：越界不报错，按上限执行，实际生效值回显在 attrs（仅钳制时出现）；
// 参数类失败自报 invalid_argument，不再落 protocol.AsFault 的 internal 兜底。

import (
	"testing"

	"github.com/veypi/aic-pod/protocol"
)

func TestRgClampsContextAndLimit(t *testing.T) {
	env := newTestEnv(t)
	runWrite(t, env, "/a.txt", "l1\nl2\nmatch\nl4\nl5\nl6\n")

	// 越界：context 14 → 10，limit 9999 → 200，均按上限执行并回显生效值
	r := runFS(t, env, map[string]any{
		"action": "rg", "path": "/a.txt", "pattern": "match",
		"context": 14, "limit": 9999,
	})
	if r.Attrs["context"] != "10" {
		t.Fatalf("context 未钳制到上限: %v", r.Attrs)
	}
	if r.Attrs["limit"] != "200" {
		t.Fatalf("limit 未钳制到上限: %v", r.Attrs)
	}
	if r.Attrs["rows"] == "" || r.Attrs["rows"] == "0" {
		t.Fatalf("钳制后应正常出结果: %v", r.Attrs)
	}

	// 负值同样钳制到下限（不报错）
	r = runFS(t, env, map[string]any{
		"action": "rg", "path": "/a.txt", "pattern": "match", "context": -3,
	})
	if r.Attrs["context"] != "0" {
		t.Fatalf("context 未钳制到下限: %v", r.Attrs)
	}

	// 未越界：不回显（参数原样生效，attrs 无噪音）
	r = runFS(t, env, map[string]any{
		"action": "rg", "path": "/a.txt", "pattern": "match", "context": 2,
	})
	if _, ok := r.Attrs["context"]; ok {
		t.Fatalf("未越界不该回显 context: %v", r.Attrs)
	}
	if _, ok := r.Attrs["limit"]; ok {
		t.Fatalf("未指定 limit 不该回显: %v", r.Attrs)
	}

	// 列举模式（pattern 缺省）同样吃 limit 钳制
	r = runFS(t, env, map[string]any{"action": "rg", "path": "/", "limit": 5000})
	if r.Attrs["limit"] != "200" {
		t.Fatalf("列举模式 limit 未钳制: %v", r.Attrs)
	}
}

func TestFsErrClassifiedAsInvalidArgument(t *testing.T) {
	// 参数/语义类失败：Error() 文案不变（§2.3 锁定），Fault 码为 invalid_argument
	err := fsErr("rg", "context is only valid for content search (pattern is required)")
	if got, want := err.Error(), "fs rg: context is only valid for content search (pattern is required)"; got != want {
		t.Fatalf("文案被改动: %q", got)
	}
	if f := protocol.AsFault(err); f.Code != "invalid_argument" {
		t.Fatalf("未归类为 invalid_argument: %+v", f)
	}
	// 能力类拒绝走 unsupported
	if f := protocol.AsFault(fsUnsupportedErr("mv", "gone")); f.Code != "unsupported" {
		t.Fatalf("未归类为 unsupported: %+v", f)
	}
	// fsOpErr（OS 层错误）保持既有链：ENOENT 仍是 not_found
	if f := protocol.AsFault(err); f.Message != err.Error() {
		t.Fatalf("Message 不该被改写: %+v", f)
	}
}
