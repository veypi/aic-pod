package host

// exec_result_test.go 锁定 exec 完成响应的输出策略（hosts-vsh-redesign §3.1 +
// 2026-09-28 用户裁定）：截断预览只针对 NATS（AI 消费）；RTC 直连全量返回
// content+attrs——不截断、不转后台、没有 truncated 标记。

import (
	"strings"
	"testing"

	vshglue "github.com/veypi/aic-pod/libs/vsh"
)

func TestExecResultResponsePolicy(t *testing.T) {
	t.Parallel()
	mkLines := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteString("line\n")
		}
		return b.String()
	}
	attrs := func() map[string]string {
		return map[string]string{"action": "exec", "output": "/o", "error_output": "/e"}
	}

	// NATS：超 1000 行 → 行截断 + truncated 标记。
	r := execResultResponse(&vshglue.ExecResult{ExitCode: 0, Stdout: mkLines(1001)}, attrs(), false)
	if r.Attrs["truncated"] != "true" {
		t.Fatal("NATS line truncation must set truncated")
	}
	if got := strings.Count(r.Content, "\n"); got != 1000 {
		t.Fatalf("NATS content lines = %d, want 1000", got)
	}

	// NATS：stderr 超 100 行 → 预览截断 + truncated；引擎采集截断也置标记。
	r = execResultResponse(&vshglue.ExecResult{ExitCode: 0, Stdout: "ok", Stderr: mkLines(101)}, attrs(), false)
	if r.Attrs["truncated"] != "true" || strings.Count(r.Attrs["stderr"], "\n") != 100 {
		t.Fatalf("NATS stderr preview: %+v", r.Attrs)
	}
	r = execResultResponse(&vshglue.ExecResult{ExitCode: 0, Stdout: "ok", StdoutTruncated: true}, attrs(), false)
	if r.Attrs["truncated"] != "true" {
		t.Fatal("NATS engine capture truncation must set truncated")
	}

	// RTC：全量返回——不截断、没有 truncated 标记（即使引擎采集截断也一样：
	// 更多数据经 fs 读日志，exec 响应不表达截断概念）。
	big := mkLines(5000)
	r = execResultResponse(&vshglue.ExecResult{ExitCode: 0, Stdout: big, Stderr: "diag", StdoutTruncated: true}, attrs(), true)
	if r.Content != big {
		t.Fatalf("RTC content must be full (got %d bytes, want %d)", len(r.Content), len(big))
	}
	if r.Attrs["stderr"] != "diag" || r.Attrs["truncated"] != "" {
		t.Fatalf("RTC attrs: %+v", r.Attrs)
	}

	// exit_code 逐码透出（两通道同）。
	r = execResultResponse(&vshglue.ExecResult{ExitCode: 42, Stdout: ""}, attrs(), true)
	if r.Attrs["exit_code"] != "42" {
		t.Fatalf("exit_code: %+v", r.Attrs)
	}
}
