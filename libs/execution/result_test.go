package execution

import (
	"strings"
	"testing"
)

// TestPreviewHeadByteCap 锁定预览字节兜底：行数未超限时，单行/少行巨输出
// 仍被截断（回退到上限内最后一个换行；无换行硬截），截取置 cut=true。
func TestPreviewHeadByteCap(t *testing.T) {
	big := strings.Repeat("x", PreviewStdoutBytes+100)
	head, cut := previewHead(big, PreviewStdoutLines, PreviewStdoutBytes)
	if !cut || len(head) != PreviewStdoutBytes {
		t.Fatalf("single huge line: len=%d cut=%v", len(head), cut)
	}
	multi := strings.Repeat("y\n", PreviewStdoutBytes/2)
	head, cut = previewHead(multi, PreviewStdoutLines, PreviewStdoutBytes)
	if !cut || len(head) > PreviewStdoutBytes || !strings.HasSuffix(head, "\n") {
		t.Fatalf("byte cut must fall back to last newline: len=%d cut=%v", len(head), cut)
	}
	small := "a\nb\n"
	if head, cut = previewHead(small, PreviewStdoutLines, PreviewStdoutBytes); cut || head != small {
		t.Fatalf("small content must pass through: %q %v", head, cut)
	}
}

// TestCapturedPreviewByteCapTruncates 端到端锁定：单行巨输出进 AI 预览时
// 带 truncated 标记（Settle 据同一谓词 spill 落盘供续读）。
func TestCapturedPreviewByteCapTruncates(t *testing.T) {
	logs := NewLogPair(t.TempDir()+"/o.log", t.TempDir()+"/e.log")
	res := &ExecResult{ExitCode: 0, Stdout: strings.Repeat("z", PreviewStdoutBytes+1)}
	out := logs.Captured(res, map[string]string{}, false)
	if out.Attrs["truncated"] != "true" {
		t.Fatal("byte-capped preview must set truncated")
	}
	if len(out.Content) > PreviewStdoutBytes {
		t.Fatalf("preview content exceeds byte cap: %d", len(out.Content))
	}
}
