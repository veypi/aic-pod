package vsh

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/veypi/vigo/contrib/ufs"
	gbfs "github.com/veypi/vsh/fs"
)

// newTestEngine cloud 形态 Engine：UFS localFS backing + jail /u/u1，无规则表。
func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	backing, err := ufs.NewLocalFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e, err := NewEngine(EngineConfig{
		// HOME=/u/{uid}、PATH 钉死（design §4.1：env 由平台每次注入）。
		BaseEnv: map[string]string{"HOME": "/u/u1", "PATH": "/usr/bin:/bin"},
		NewSessionFS: func(key string) (gbfs.FileSystem, string, error) {
			fsys, err := NewCloudFS(CloudFSConfig{UserRoot: "/u/u1", Backing: backing})
			return fsys, "/u/u1", err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestEngineExecBasic(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t)
	res, err := e.Exec(context.Background(), ExecRequest{SessionKey: "s1", Script: "echo hello; echo err >&2"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != "hello" {
		t.Fatalf("got code=%d stdout=%q", res.ExitCode, res.Stdout)
	}
	if strings.TrimSpace(res.Stderr) != "err" {
		t.Fatalf("stderr=%q", res.Stderr)
	}
}

func TestEngineSessionPersistsFS(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t)
	ctx := context.Background()
	// UFS 直通语义：落 backing 的写跨会话可见（UFS 是持久层）。
	if _, err := e.Exec(ctx, ExecRequest{SessionKey: "s1", Script: "echo data > f.txt"}); err != nil {
		t.Fatal(err)
	}
	res, err := e.Exec(ctx, ExecRequest{SessionKey: "s1", Script: "cat f.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Stdout) != "data" {
		t.Fatalf("session fs not shared: %q", res.Stdout)
	}
	res2, err := e.Exec(ctx, ExecRequest{SessionKey: "s2", Script: "cat f.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res2.Stdout) != "data" {
		t.Fatalf("UFS should persist across sessions: %q", res2.Stdout)
	}
	// 内存层 per-session 隔离（验收 7）：s1 的 /tmp 对 s2 不可见。
	if _, err := e.Exec(ctx, ExecRequest{SessionKey: "s1", Script: "echo tmp > /tmp/mem-only.txt"}); err != nil {
		t.Fatal(err)
	}
	res3, err := e.Exec(ctx, ExecRequest{SessionKey: "s2", Script: "cat /tmp/mem-only.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if res3.ExitCode == 0 {
		t.Fatalf("mem layer should be per-session isolated")
	}
}

func TestEngineRegistryHasPlatformCommands(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t)
	for _, name := range []string{"commands", "bg", "grant", "list_hosts", "send_user", "jq", "ls", "rg", "cp", "mv", "rm"} {
		if _, ok := e.Registry().Lookup(name); !ok {
			t.Fatalf("registry missing %q", name)
		}
	}
	// 内建 --help 冒烟（M1 全覆盖的回归保险丝）。
	res, err := e.Exec(context.Background(), ExecRequest{SessionKey: "s1", Script: "ls --help >/dev/null && jq --help >/dev/null && commands | grep -q '^grant$'"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("help smoke failed: code=%d stderr=%s", res.ExitCode, res.Stderr)
	}
}

func TestEngineTimeoutCap(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t)
	start := time.Now()
	res, err := e.Exec(context.Background(), ExecRequest{
		SessionKey: "s1", Script: "sleep 5", Timeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 124 {
		t.Fatalf("timeout exit = %d, want 124", res.ExitCode)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("timeout not honored")
	}
}

func TestEngineLogTee(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t)
	var log strings.Builder
	res, err := e.Exec(context.Background(), ExecRequest{
		SessionKey: "s1", Script: "echo teed", Log: &log,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "teed") || !strings.Contains(res.Stdout, "teed") {
		t.Fatalf("tee broken: log=%q stdout=%q", log.String(), res.Stdout)
	}
}

func TestEngineBGClosedLoop(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t)
	ctx := context.Background()
	// bg run → list → wait 闭环（验收 6）。
	res, err := e.Exec(ctx, ExecRequest{SessionKey: "s1", Script: "bg run 'echo bgout; sleep 0.2'"})
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(res.Stdout)
	if !strings.HasPrefix(id, "bg-") {
		t.Fatalf("bg run id = %q", id)
	}
	res, err = e.Exec(ctx, ExecRequest{SessionKey: "s1", Script: "bg wait " + id + " 10"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Stdout, "bgout") || !strings.Contains(res.Stdout, "done") {
		t.Fatalf("bg wait = %q", res.Stdout)
	}
	res, err = e.Exec(ctx, ExecRequest{SessionKey: "s1", Script: "bg list"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Stdout, id) {
		t.Fatalf("bg list missing %s: %q", id, res.Stdout)
	}
}

func TestEngineBGKill(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t)
	ctx := context.Background()
	res, err := e.Exec(ctx, ExecRequest{SessionKey: "s1", Script: "bg run 'sleep 60'"})
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(res.Stdout)
	if _, err := e.Exec(ctx, ExecRequest{SessionKey: "s1", Script: "bg kill " + id}); err != nil {
		t.Fatal(err)
	}
	task, err := e.Tasks.Wait(ctx, id, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != "killed" {
		t.Fatalf("status = %s, want killed", task.Status)
	}
}

func TestEngineWriteAudit(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t)
	res, err := e.Exec(context.Background(), ExecRequest{SessionKey: "s1", Script: "echo x > audit-me.txt"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range res.Writes {
		if strings.HasSuffix(w, "audit-me.txt") {
			found = true
		}
	}
	if !found {
		t.Fatalf("write audit missing: %v", res.Writes)
	}
}
