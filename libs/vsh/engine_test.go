package vsh

import (
	"context"
	"io"
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

// TestTaskTableCapacity 容量闸（2026-09-24 用户拍板：全局 + per-owner 上限，
// 超额快速拒绝——bg fan-out/多会话并发 yes 可占满全部核的防线）。
func TestTaskTableCapacity(t *testing.T) {
	t.Parallel()
	tt := NewTaskTableWithCaps(3, 2)
	block := func(ctx context.Context, log io.Writer) (int, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	start := func(owner string) (Task, error) {
		return tt.Start("t", "", owner, block, nil)
	}
	// per-owner=2：o1 第 3 个拒。
	if _, err := start("o1"); err != nil {
		t.Fatal(err)
	}
	k2, err := start("o1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := start("o1"); err == nil || !strings.Contains(err.Error(), "单归属") {
		t.Fatalf("per-owner 超额应拒: %v", err)
	}
	// o2 第 1 个过（此时 running=3 达全局），o2 第 2 个撞全局拒。
	if _, err := start("o2"); err != nil {
		t.Fatal(err)
	}
	if _, err := start("o2"); err == nil || !strings.Contains(err.Error(), "全局") {
		t.Fatalf("全局超额应拒: %v", err)
	}
	// kill 释放后容量恢复。
	if err := tt.Kill(k2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tt.Wait(context.Background(), k2.ID, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := start("o1"); err != nil {
		t.Fatalf("释放后应可启动: %v", err)
	}
}

// TestTaskTableKillReleasesImmediately kill 即释放容量：不等 goroutine 收尾，
// kill 后紧接着 Start 不撞 full（救场路径：杀失控任务后立刻起新任务）。
func TestTaskTableKillReleasesImmediately(t *testing.T) {
	t.Parallel()
	tt := NewTaskTableWithCaps(2, 2)
	block := func(ctx context.Context, log io.Writer) (int, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	k1, err := tt.Start("t", "", "o1", block, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tt.Start("t", "", "o1", block, nil); err != nil {
		t.Fatal(err)
	}
	if err := tt.Kill(k1.ID); err != nil {
		t.Fatal(err)
	}
	// 不 Wait、不睡觉：容量必须已同步释放。
	if _, err := tt.Start("t", "", "o1", block, nil); err != nil {
		t.Fatalf("kill 后应立即可补位: %v", err)
	}
	// 收尾后计数不双重释放（goroutine 退出时 released 已置位）。
	if _, err := tt.Wait(context.Background(), k1.ID, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	tt.mu.Lock()
	defer tt.mu.Unlock()
	if tt.running != 2 || tt.runningByOwner["o1"] != 2 {
		t.Fatalf("running=%d byOwner=%v, want 2/2（双重释放？）", tt.running, tt.runningByOwner)
	}
}

// TestTaskTableEviction 完成任务表保留上限：超出逐出最旧（每项带 8MiB 上限
// 缓冲，不限量累积是内存泄漏面）。
func TestTaskTableEviction(t *testing.T) {
	t.Parallel()
	tt := NewTaskTableWithCaps(16, 16)
	tt.maxRetained = 3
	quick := func(ctx context.Context, log io.Writer) (int, error) { return 0, nil }
	var last Task
	for i := 0; i < 6; i++ {
		task, err := tt.Start("t", "", "o1", quick, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tt.Wait(context.Background(), task.ID, 3*time.Second); err != nil {
			t.Fatal(err)
		}
		last = task
	}
	tasks := tt.List()
	if len(tasks) > 4 { // maxRetained 3 + 最后一个 running/finished
		t.Fatalf("retained = %d, want <= 4", len(tasks))
	}
	// 最新的必须在，最旧的 bg-1/bg-2 已逐出。
	if _, ok := tt.Get(last.ID); !ok {
		t.Fatalf("latest task %s evicted", last.ID)
	}
	if _, ok := tt.Get("bg-1"); ok {
		t.Fatal("oldest bg-1 should be evicted")
	}
}

// TestBGRunCapacityEndToEnd bg run 经 Exec 继承 owner 并受容量闸约束（e2e：
// 生产表 per-owner=4，第 5 个 bg run 拒绝且报错可行动）。
func TestBGRunCapacityEndToEnd(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t)
	ctx := context.Background()
	for i := 0; i < MaxRunningTasksPerOwner; i++ {
		res, err := e.Exec(ctx, ExecRequest{SessionKey: "s1", Owner: "u:t1", Script: "bg run 'sleep 60'"})
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("bg run #%d: %+v err=%v", i+1, res, err)
		}
	}
	res, err := e.Exec(ctx, ExecRequest{SessionKey: "s1", Owner: "u:t1", Script: "bg run 'sleep 60'"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == 0 || !strings.Contains(res.Stderr, "task table full") {
		t.Fatalf("第 5 个 bg run 应拒: exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	// 另一 owner 不受 o1 占满影响。
	res, err = e.Exec(ctx, ExecRequest{SessionKey: "s2", Owner: "u:t2", Script: "bg run 'sleep 1'"})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("异 owner bg run 应过: %+v err=%v", res, err)
	}
}

// TestBGOutput bg output 子命令：不等待直取任务捕获输出。
func TestBGOutput(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t)
	ctx := context.Background()
	res, err := e.Exec(ctx, ExecRequest{SessionKey: "s1", Script: "bg run 'echo hello-bg'"})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("bg run: %+v err=%v", res, err)
	}
	tid := strings.TrimSpace(res.Stdout)
	if _, err := e.Tasks.Wait(ctx, tid, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	res, err = e.Exec(ctx, ExecRequest{SessionKey: "s1", Script: "bg output " + tid})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Stdout, "hello-bg") {
		t.Fatalf("bg output = %q", res.Stdout)
	}
}

// TestIsPureBGMgmtScript 管理面快路径判定：仅单条纯 bg list/wait/kill/output
// （或裸 bg）放行；bg run（cwd 继承依赖基会话）与一切复合/动态形态排除。
func TestIsPureBGMgmtScript(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"bg":                      true,
		"bg list":                 true,
		"bg kill bg-3":            true,
		"bg wait bg-3 5":          true,
		"bg output bg-3":          true,
		"  bg   kill   bg-3  ":    true,
		"bg run 'sleep 1'":        false, // cwd 继承依赖基会话，非救场命令
		"bg list; echo done":      false,
		"bg list | grep bg":       false,
		"bg list > out.txt":       false,
		"bg kill bg-3 &":          false,
		"echo bg list":            false,
		"x=1 bg list":             false,
		"bg $SUB":                 false,
		"bg kill $(cat /tmp/id)":  false,
		"grant fs /u/u1/x":        false,
		"bg kill bg-3 # 注释":     true,
	}
	for script, want := range cases {
		if got := isPureBGMgmtScript(script); got != want {
			t.Errorf("isPureBGMgmtScript(%q) = %v, want %v", script, got, want)
		}
	}
}

// TestBGMgmtBypassesSessionLock 管理面快路径 e2e：基会话被长任务持锁时，
// 纯 bg 管理命令仍即时到达执行层（2026-09-24 实测会话活锁漏洞：前台长任务
// 超时转 bg 后仍持基会话执行锁，救场的 bg kill 排队到不了执行层）。
func TestBGMgmtBypassesSessionLock(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t)
	ctx := context.Background()

	// 先起一个 bg 长任务供 kill。
	res, err := e.Exec(ctx, ExecRequest{SessionKey: "s1", Script: "bg run 'sleep 60'"})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("bg run: %+v err=%v", res, err)
	}
	tid := strings.TrimSpace(res.Stdout)

	// 基会话长任务持锁（模拟前台超时转 bg 后仍占位的执行，sleep 10 >> 快路径阈值）。
	go func() {
		_, _ = e.Exec(ctx, ExecRequest{SessionKey: "s1", Script: "sleep 10", LongRunning: true, Timeout: BackgroundWallClock})
	}()
	time.Sleep(500 * time.Millisecond) // 等基会话锁被占

	// kill 应即时执行（不排基会话锁），任务被杀。
	start := time.Now()
	res, err = e.Exec(ctx, ExecRequest{SessionKey: "s1", Script: "bg kill " + tid})
	if err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("bg kill blocked %v（排在基会话锁后）", el)
	}
	if res.ExitCode != 0 {
		t.Fatalf("bg kill exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	task, err := e.Tasks.Wait(ctx, tid, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != "killed" {
		t.Fatalf("task status = %q, want killed", task.Status)
	}

	// list 同样快路径，且能看到任务表。
	start = time.Now()
	res, err = e.Exec(ctx, ExecRequest{SessionKey: "s1", Script: "bg list"})
	if err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("bg list blocked %v", el)
	}
	if !strings.Contains(res.Stdout, tid) {
		t.Fatalf("bg list missing %s: %q", tid, res.Stdout)
	}
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
