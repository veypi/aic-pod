package host

// exec_result_test.go 锁定 exec 完成响应的输出策略（hosts-vsh-redesign §3.1 +
// 2026-09-28 用户裁定）：截断预览只针对 NATS（AI 消费）；RTC 直连全量返回
// content+attrs——不截断、不转后台、没有 truncated 标记。

import (
	"context"
	"encoding/json"
	"github.com/veypi/aic-pod/protocol"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/execution"
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
	r := execResultResponse(&execution.ExecResult{ExitCode: 0, Stdout: mkLines(1001)}, attrs(), false)
	if r.Attrs["truncated"] != "true" {
		t.Fatal("NATS line truncation must set truncated")
	}
	if got := strings.Count(r.Content, "\n"); got != 1000 {
		t.Fatalf("NATS content lines = %d, want 1000", got)
	}

	// NATS：stderr 超 100 行 → 预览截断 + truncated；引擎采集截断也置标记。
	r = execResultResponse(&execution.ExecResult{ExitCode: 0, Stdout: "ok", Stderr: mkLines(101)}, attrs(), false)
	if r.Attrs["truncated"] != "true" || strings.Count(r.Attrs["stderr"], "\n") != 100 {
		t.Fatalf("NATS stderr preview: %+v", r.Attrs)
	}
	r = execResultResponse(&execution.ExecResult{ExitCode: 0, Stdout: "ok", StdoutTruncated: true}, attrs(), false)
	if r.Attrs["truncated"] != "true" {
		t.Fatal("NATS engine capture truncation must set truncated")
	}

	// RTC formatter receives full streams after completion recovery. Successful
	// responses have no truncated marker; recovery failures are explicit errors.
	big := mkLines(5000)
	r = execResultResponse(&execution.ExecResult{ExitCode: 0, Stdout: big, Stderr: "diag"}, attrs(), true)
	if r.Content != big {
		t.Fatalf("RTC content must be full (got %d bytes, want %d)", len(r.Content), len(big))
	}
	if r.Attrs["stderr"] != "diag" || r.Attrs["truncated"] != "" {
		t.Fatalf("RTC attrs: %+v", r.Attrs)
	}

	// exit_code 逐码透出（两通道同）。
	r = execResultResponse(&execution.ExecResult{ExitCode: 42, Stdout: ""}, attrs(), true)
	if r.Attrs["exit_code"] != "42" {
		t.Fatalf("exit_code: %+v", r.Attrs)
	}
}

func TestCompletedExecResultRestoresStderrBeforeAudit(t *testing.T) {
	t.Parallel()
	stdout, err := os.Create(filepath.Join(t.TempDir(), "stdout.log"))
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.Create(filepath.Join(t.TempDir(), "stderr.log"))
	if err != nil {
		_ = stdout.Close()
		t.Fatal(err)
	}
	outText, errText := `{"ok":true}`, "complete diagnostic\n"
	if _, err := stdout.WriteString(outText); err != nil {
		t.Fatal(err)
	}
	if _, err := stderr.WriteString(errText); err != nil {
		t.Fatal(err)
	}
	res := &execution.ExecResult{
		ExitCode: 42, Stdout: outText[:4], Stderr: errText[:4],
		StdoutTruncated: true, StderrTruncated: true, Writes: []string{"/created.txt"},
	}
	logs := closeExecLogs(stdout, stderr, res)
	attrs := map[string]string{"action": "exec", "output": stdout.Name(), "error_output": stderr.Name()}
	got, err := completedExecResultResponse(res, attrs, true, logs)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != outText || got.Attrs["stderr"] != errText || got.Attrs["exit_code"] != "42" || got.Attrs["truncated"] != "" {
		t.Fatalf("restored response = %+v", got)
	}
	if !res.StdoutTruncated || res.Stdout != outText[:4] || !res.StderrTruncated || res.Stderr != errText[:4] {
		t.Fatal("recovery mutated the engine result")
	}
	audit, err := os.ReadFile(stderr.Name())
	if err != nil || !strings.Contains(string(audit), "# vsh fs writes") || !strings.HasPrefix(string(audit), errText) {
		t.Fatalf("stderr audit log = %q, %v", audit, err)
	}
}

func TestCompletedExecResultRecoveryFailures(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "short.log")
	if err := os.WriteFile(path, []byte("short"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path, code string
		res              execution.ExecResult
		logs             execLogSnapshot
	}{
		{"stdout-limit", path, "overloaded", execution.ExecResult{Stdout: "prefix", StdoutTruncated: true}, execLogSnapshot{stdoutBytes: protocol.RtcToolResponseLimit + 1}},
		{"combined-limit", path, "overloaded", execution.ExecResult{Stdout: "prefix", Stderr: "!", StdoutTruncated: true}, execLogSnapshot{stdoutBytes: protocol.RtcToolResponseLimit}},
		{"missing", path + ".missing", "internal", execution.ExecResult{Stdout: "prefix", StdoutTruncated: true}, execLogSnapshot{stdoutBytes: 10}},
		{"file-shortened", path, "internal", execution.ExecResult{Stdout: "prefix", StdoutTruncated: true}, execLogSnapshot{stdoutBytes: 10}},
		{"snapshot-shorter-than-capture", path, "internal", execution.ExecResult{Stdout: "prefix", StdoutTruncated: true}, execLogSnapshot{stdoutBytes: 5}},
		{"stderr-missing", path + ".missing", "internal", execution.ExecResult{Stdout: "ok", Stderr: "prefix", StderrTruncated: true}, execLogSnapshot{stderrBytes: 10}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attrs := map[string]string{"action": "exec", "output": tc.path, "error_output": tc.path}
			got, err := completedExecResultResponse(&tc.res, attrs, true, tc.logs)
			if err == nil || protocol.AsFault(err).Code != tc.code {
				t.Fatalf("error = %v, want %s", err, tc.code)
			}
			if got.Content != "" || got.Attrs["stderr"] != "" || got.Attrs["truncated"] != "" || got.Attrs["output"] != tc.path || got.Attrs["error_output"] != tc.path || got.Attrs["exit_code"] != "0" {
				t.Fatalf("failure must retain metadata without partial payload: %+v", got)
			}
		})
	}
}

func TestCompletedExecResultNATSDoesNotReadLogs(t *testing.T) {
	t.Parallel()
	res := &execution.ExecResult{Stdout: "captured", Stderr: "diagnostic", StdoutTruncated: true, StderrTruncated: true}
	attrs := map[string]string{"action": "exec", "output": "/missing", "error_output": "/missing"}
	got, err := completedExecResultResponse(res, attrs, false, execLogSnapshot{stdoutBytes: protocol.RtcToolResponseLimit + 1, err: os.ErrNotExist})
	if err != nil || got.Content != "captured" || got.Attrs["stderr"] != "diagnostic" || got.Attrs["truncated"] != "true" {
		t.Fatalf("NATS must keep bounded capture without reading logs: response=%+v, error=%v", got, err)
	}
}

// Exercise the actual vsh capture, full log passthrough, onDone snapshot and host
// response together. This exceeds the 8 MiB capture while staying within the
// bounded RTC response allowance.
func TestExecScriptRTCRestoresLargeJSON(t *testing.T) {
	saved := cfg.Global
	cfg.Global = cfg.NewOptions()
	t.Cleanup(func() { cfg.Global = saved })
	c, _ := testClient(t)
	stdout := `{"image":"` + strings.Repeat("A", 10<<20) + `"}`
	stderr := strings.Repeat("diagnostic\n", (1<<20)/11+1)
	for name, data := range map[string]string{"large.json": stdout, "large.err": stderr} {
		if err := os.WriteFile(filepath.Join(c.options().WorkDir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	script := "cat large.json; cat large.err >&2"
	caller := testCaller()
	caller.Direct = true
	r := callTool(t, c, context.Background(), caller, execRequest(script, 30000))
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	got, ok := r.Result.(*protocol.Output)
	if !ok {
		t.Fatalf("result type = %T", r.Result)
	}
	if got.Content != stdout || !json.Valid([]byte(got.Content)) || got.Attrs["stderr"] != stderr || got.Attrs["exit_code"] != "0" || got.Attrs["truncated"] != "" {
		t.Fatalf("RTC incomplete: stdout bytes=%d, stderr bytes=%d, exit=%q, truncated=%q", len(got.Content), len(got.Attrs["stderr"]), got.Attrs["exit_code"], got.Attrs["truncated"])
	}
	log, err := os.ReadFile(got.Attrs["output"])
	if err != nil || string(log) != stdout {
		t.Fatalf("full stdout log: bytes=%d, error=%v", len(log), err)
	}

	// NATS still uses the existing 8 MiB engine capture and short stderr preview.
	r = signedCall(t, c, execRequest(script, 30000), false, "s1", "")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	got, ok = r.Result.(*protocol.Output)
	if !ok {
		t.Fatalf("NATS result type = %T", r.Result)
	}
	if len(got.Content) != execution.MaxStdoutBytes || !strings.HasPrefix(stdout, got.Content) || strings.Count(got.Attrs["stderr"], "\n") != 100 || got.Attrs["truncated"] != "true" {
		t.Fatalf("NATS policy changed: stdout bytes=%d, stderr lines=%d, truncated=%q", len(got.Content), strings.Count(got.Attrs["stderr"], "\n"), got.Attrs["truncated"])
	}
}
