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

// TestEngineSessionKeyInContext Exec 把会话键注入 ctx（M3c：NetClient
// per-sid 规则表与平台命令共用此通道）——平台 grant 回调应能取到。
func TestEngineSessionKeyInContext(t *testing.T) {
	t.Parallel()
	backing, err := ufs.NewLocalFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var gotSid string
	e, err := NewEngine(EngineConfig{
		BaseEnv: map[string]string{"HOME": "/u/u1", "PATH": "/usr/bin:/bin"},
		NewSessionFS: func(key string) (gbfs.FileSystem, string, error) {
			fsys, err := NewCloudFS(CloudFSConfig{UserRoot: "/u/u1", Backing: backing})
			return fsys, "/u/u1", err
		},
		Platform: PlatformDeps{
			Grant: func(ctx context.Context, sessionKey, domain, target string) (string, error) {
				gotSid = SessionFromContext(ctx)
				return "ok", nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Exec(context.Background(), ExecRequest{SessionKey: "sid-42", Script: "grant fs /u/u1/x"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("grant exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if gotSid != "sid-42" {
		t.Fatalf("SessionFromContext = %q, want sid-42", gotSid)
	}
}

// TestCloudJailAncestorMeta 2026-09-24 实测修复回归：内建命令的祖先链走访
//（mkdir -p 逐级 Stat、cd/ls 符号链接解析）读 jail 根祖先（/、/u）放行
// 元数据；界外内容读写、根列表仍硬拒。mkdir 收尾 Chmod 在无权限位的 UFS
// backing 上 noop（不假失败）。
func TestCloudJailAncestorMeta(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t)
	type tc struct {
		script   string
		wantCode int
		wantOut  string // stdout 包含
		wantErr  string // stderr 包含
	}
	for _, c := range []tc{
		{"mkdir /u/u1/aa && echo ok", 0, "ok", ""},
		{"mkdir -p /u/u1/a/b/c && echo ok", 0, "ok", ""},
		{"cd /u/u1 && pwd", 0, "/u/u1", ""},
		{"cp -r /u/u1/a /u/u1/acopy && echo ok", 0, "ok", ""},
		{"ls /u/u1", 0, "acopy", ""},
		// /dev 虚拟设备：写丢弃、不到达适配器（静态预检同步豁免）。
		{"echo x > /dev/null && echo ok", 0, "ok", ""},
		{"ls /nonexistent 2>/dev/null; echo code=$?", 0, "code=2", ""},
		// /dev/stdout|stderr 是 runner fd 别名（per-exec 流），与 bash 一致。
		{"echo out > /dev/stdout && echo err > /dev/stderr", 0, "out", "err"},
		// 拒绝面（放宽不漏）：界外读/写、别人用户根、根列表。
		{"mkdir /u/other/x", 1, "", "jail"},
		{"cat /u/other/secret", 1, "", "jail"},
		{"ls /", 1, "", ""},
		{"echo x > /x.txt", 1, "", "jail"},
	} {
		res, err := e.Exec(context.Background(), ExecRequest{SessionKey: "s1", Script: c.script})
		if err != nil {
			t.Fatalf("%q → engine err %v", c.script, err)
		}
		if res.ExitCode != c.wantCode {
			t.Errorf("%q → exit=%d, want %d（stderr=%q）", c.script, res.ExitCode, c.wantCode, res.Stderr)
		}
		if c.wantOut != "" && !strings.Contains(res.Stdout, c.wantOut) {
			t.Errorf("%q → stdout=%q, want 含 %q", c.script, res.Stdout, c.wantOut)
		}
		if c.wantErr != "" && !strings.Contains(res.Stderr+res.Stdout, c.wantErr) {
			t.Errorf("%q → 输出不含 %q（stderr=%q）", c.script, c.wantErr, res.Stderr)
		}
	}
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
