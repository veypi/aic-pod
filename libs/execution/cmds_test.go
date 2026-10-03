package execution

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/veypi/vsh/commands"
)

// runCmdCtx 以给定 ctx 执行平台命令（测试可信上下文注入用）。
func runCmdCtx(ctx context.Context, t *testing.T, reg *commands.Registry, name string, args ...string) (string, string, error) {
	t.Helper()
	cmd, ok := reg.Lookup(name)
	if !ok {
		t.Fatalf("command %q not registered", name)
	}
	var out, errBuf bytes.Buffer
	inv := &commands.Invocation{
		Args:   args,
		Env:    map[string]string{},
		Stdout: &out,
		Stderr: &errBuf,
		GetRegisteredCommands: func() []string {
			return reg.Names()
		},
	}
	err := commands.RunCommand(ctx, cmd, inv)
	return out.String(), errBuf.String(), err
}

func runCmd(t *testing.T, reg *commands.Registry, name string, args ...string) (string, string, error) {
	t.Helper()
	return runCmdCtx(context.Background(), t, reg, name, args...)
}

// identityCtx 注入任务归属（user/session）与其他可信事实。
func identityCtx(owner, session string, grantApproved bool) context.Context {
	ctx := context.Background()
	ctx = context.WithValue(ctx, ownerKey{}, owner)
	ctx = context.WithValue(ctx, netSessionKey{}, session)
	ctx = context.WithValue(ctx, grantApprovedKey{}, grantApproved)
	return ctx
}

func TestPlatformCommandsRegistered(t *testing.T) {
	t.Parallel()
	reg := commands.NewRegistry()
	if err := RegisterPlatformCommands(reg, PlatformDeps{Tasks: NewTaskTable()}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"commands", "bg"} {
		if _, ok := reg.Lookup(name); !ok {
			t.Fatalf("missing %q", name)
		}
	}
	for _, name := range []string{"grant", "list_hosts", "send_user", "skill"} {
		if _, ok := reg.Lookup(name); ok {
			t.Fatalf("unsupported command %q registered", name)
		}
	}
}

func TestCmdCommandsListsRegistry(t *testing.T) {
	t.Parallel()
	reg := commands.NewRegistry()
	_ = RegisterPlatformCommands(reg, PlatformDeps{Tasks: NewTaskTable()})
	out, _, err := runCmd(t, reg, "commands")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "commands") || !strings.Contains(out, "bg") {
		t.Fatalf("commands out = %q", out)
	}
}

// commands 展示过滤（§2.2）：host/cloud 只展示核心自定义指令；未列出
// 不影响执行（展示过滤不是白名单）。
func TestCmdCommandsDisplayFilter(t *testing.T) {
	t.Parallel()
	reg := commands.NewRegistry()
	core := map[string]bool{"commands": true, "bg": true, "grant": true}
	_ = RegisterPlatformCommands(reg, PlatformDeps{
		Tasks:        NewTaskTable(),
		Discoverable: func(name string) bool { return core[name] },
		SendUser:     func(context.Context, string, string) error { return nil },
	})
	out, _, err := runCmd(t, reg, "commands")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "list_hosts") || strings.Contains(out, "send_user") {
		t.Fatalf("filtered out = %q", out)
	}
	if !strings.Contains(out, "commands") {
		t.Fatalf("commands out = %q", out)
	}
	// 未列出照常可执行
	if _, _, err := runCmd(t, reg, "send_user", "hello"); err != nil {
		t.Fatal(err)
	}
}

// grant 修改入口是安全边界：无 grant_approved 一律 permission_denied
// （规则不变）；status/help 不要求批准。
func TestCmdGrantRequiresApprovalFact(t *testing.T) {
	t.Parallel()
	reg := commands.NewRegistry()
	called := false
	deps := PlatformDeps{
		Tasks: NewTaskTable(),
		Grant: func(ctx context.Context, sessionKey, domain, target string, permanent bool) (string, error) {
			called = true
			return "已授权", nil
		},
		GrantStatus: func(ctx context.Context, sessionKey string) (string, error) {
			return "fs_policy: deny", nil
		},
	}
	_ = RegisterPlatformCommands(reg, deps)
	// 无批准事实：拒绝且不触达 Grant
	_, _, err := runCmdCtx(identityCtx("u1", "s1", false), t, reg, "grant", "fs", "/x")
	if err == nil || !strings.Contains(err.Error(), "permission_denied") {
		t.Fatalf("unapproved grant must be denied: %v", err)
	}
	if called {
		t.Fatal("Grant called without grant_approved")
	}
	// status 只读：不要求批准
	out, _, err := runCmdCtx(identityCtx("u1", "s1", false), t, reg, "grant", "status")
	if err != nil || !strings.Contains(out, "fs_policy") {
		t.Fatalf("status: %q %v", out, err)
	}
	for _, args := range [][]string{nil, {"--help"}, {"-h"}, {"help"}, {"status"}, {"status", "--help"}} {
		if out, _, err := runCmdCtx(identityCtx("u1", "s1", false), t, reg, "grant", args...); err != nil || out == "" {
			t.Fatalf("read-only grant %v: %q %v", args, out, err)
		}
		if called {
			t.Fatalf("read-only grant %v modified permissions", args)
		}
	}
	// 批准事实：执行
	out, _, err = runCmdCtx(identityCtx("u1", "s1", true), t, reg, "grant", "fs", "/x")
	if err != nil || !strings.Contains(out, "已授权") || !called {
		t.Fatalf("approved grant: %q %v called=%v", out, err, called)
	}
}

func TestCmdGrantFlow(t *testing.T) {
	t.Parallel()
	reg := commands.NewRegistry()
	var gotDomain, gotTarget string
	var gotPermanent bool
	deps := PlatformDeps{
		Tasks: NewTaskTable(),
		Grant: func(ctx context.Context, sessionKey, domain, target string, permanent bool) (string, error) {
			gotDomain, gotTarget, gotPermanent = domain, target, permanent
			return "已授权", nil
		},
	}
	_ = RegisterPlatformCommands(reg, deps)
	ctx := identityCtx("u1", "s1", true)
	out, _, err := runCmdCtx(ctx, t, reg, "grant", "fs", "/u/u1/docs")
	if err != nil {
		t.Fatal(err)
	}
	if gotDomain != "fs" || gotTarget != "/u/u1/docs" || gotPermanent || !strings.Contains(out, "已授权") {
		t.Fatalf("grant: %q %q %v %q", gotDomain, gotTarget, gotPermanent, out)
	}
	// ssh 域 + --permanent（任意位置）透传。
	if _, _, err = runCmdCtx(ctx, t, reg, "grant", "ssh", "example.com:22", "--permanent"); err != nil {
		t.Fatal(err)
	}
	if gotDomain != "ssh" || gotTarget != "example.com:22" || !gotPermanent {
		t.Fatalf("grant ssh --permanent: %q %q %v", gotDomain, gotTarget, gotPermanent)
	}
	if _, _, err = runCmdCtx(ctx, t, reg, "grant", "--permanent", "net", "example.com:443"); err != nil {
		t.Fatal(err)
	}
	if gotDomain != "net" || !gotPermanent {
		t.Fatalf("grant --permanent net: %q %v", gotDomain, gotPermanent)
	}
	// 未知域报错。
	_, _, err = runCmdCtx(ctx, t, reg, "grant", "root", "/")
	if err == nil {
		t.Fatal("unknown domain should fail")
	}
	// help 自答（不要求批准）。
	out, _, err = runCmd(t, reg, "grant", "--help")
	if err != nil || !strings.Contains(out, "grant fs") {
		t.Fatalf("help: %q %v", out, err)
	}
}

// bg 闭环（新模型）：任务唯一来源 = Adopt（exec 前台等待超时登记）；
// list/wait/kill 按 (owner, session) 归属。
func TestCmdBGClosedLoop(t *testing.T) {
	t.Parallel()
	tasks := NewTaskTable()
	reg := commands.NewRegistry()
	_ = RegisterPlatformCommands(reg, PlatformDeps{Tasks: tasks})
	ctx := identityCtx("u1", "s1", false)

	// 空表
	out, _, err := runCmdCtx(ctx, t, reg, "bg", "list")
	if err != nil || !strings.Contains(out, "(no background tasks)") {
		t.Fatalf("empty list: %q %v", out, err)
	}

	// Adopt 一个已完成的执行：登记被拒（竞争回退为完成结果）
	h := NewExecHandle(func() {})
	h.Finish(&ExecResult{ExitCode: 0}, nil)
	if _, err := tasks.Adopt(h, "echo hi", TaskMeta{Owner: "u1", Session: "s1", LogOut: "/log/out", LogErr: "/log/err"}); err == nil {
		t.Fatal("adopt of finished execution must be rejected")
	}

	h2 := NewExecHandle(func() {})
	release := make(chan struct{})
	go func() { <-release; h2.Finish(&ExecResult{ExitCode: 0}, nil) }()
	task2, err := tasks.Adopt(h2, "sleep 1", TaskMeta{Owner: "u1", Session: "s1", LogOut: "/log/o2", LogErr: "/log/e2"})
	if err != nil {
		t.Fatal(err)
	}
	// list 可见（含日志路径）
	out, _, err = runCmdCtx(ctx, t, reg, "bg", "list")
	if err != nil || !strings.Contains(out, task2.ID) || !strings.Contains(out, "/log/o2") {
		t.Fatalf("list = %q %v", out, err)
	}
	// --json 契约形状
	out, _, err = runCmdCtx(ctx, t, reg, "bg", "list", "--json")
	if err != nil || !strings.Contains(out, `"state":"running"`) || !strings.Contains(out, `"output":"/log/o2"`) {
		t.Fatalf("list --json = %q %v", out, err)
	}
	// wait 完成（有界等待共享前台预算——无预算注入时显式秒数也只查询）
	close(release)
	waitCtx := context.WithValue(ctx, waitBudgetKey{}, time.Now().Add(10*time.Second))
	out, _, err = runCmdCtx(waitCtx, t, reg, "bg", "wait", task2.ID, "5")
	if err != nil || !strings.Contains(out, "done") {
		t.Fatalf("wait = %q %v", out, err)
	}
	// wait --json 已完成带 exit_code
	out, _, err = runCmdCtx(ctx, t, reg, "bg", "wait", task2.ID, "--json")
	if err != nil || !strings.Contains(out, `"exit_code":0`) {
		t.Fatalf("wait --json = %q %v", out, err)
	}
	// 归属隔离：跨会话/跨用户不可见不可管
	other := identityCtx("u1", "s2", false)
	if _, _, err := runCmdCtx(other, t, reg, "bg", "wait", task2.ID, "0"); err == nil {
		t.Fatal("cross-session wait must fail")
	}
	if _, _, err := runCmdCtx(other, t, reg, "bg", "kill", task2.ID); err == nil {
		t.Fatal("cross-session kill must fail")
	}
	// kill 不存在任务报错
	if _, _, err = runCmdCtx(ctx, t, reg, "bg", "kill", "bg-999"); err == nil {
		t.Fatal("kill unknown should fail")
	}
	// bg run/output 已删除
	if _, _, err = runCmdCtx(ctx, t, reg, "bg", "run", "echo hi"); err == nil {
		t.Fatal("bg run must be gone")
	}
	if _, _, err = runCmdCtx(ctx, t, reg, "bg", "output", task2.ID); err == nil {
		t.Fatal("bg output must be gone")
	}
}

// bg kill 终止同一执行句柄（与 cancel 共用）。
func TestCmdBGKillCancelsHandle(t *testing.T) {
	t.Parallel()
	tasks := NewTaskTable()
	reg := commands.NewRegistry()
	_ = RegisterPlatformCommands(reg, PlatformDeps{Tasks: tasks})
	ctx := identityCtx("u1", "s1", false)

	cancelled := false
	h := NewExecHandle(func() { cancelled = true })
	go func() { <-time.After(10 * time.Second); h.Finish(&ExecResult{}, nil) }()
	task, err := tasks.Adopt(h, "sleep 10", TaskMeta{Owner: "u1", Session: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := runCmdCtx(ctx, t, reg, "bg", "kill", task.ID)
	if err != nil || !strings.Contains(out, "killed") || !cancelled {
		t.Fatalf("kill = %q %v cancelled=%v", out, err, cancelled)
	}
	snap, ok := tasks.Get(task.ID, "u1", "s1")
	if !ok || snap.Status != "killed" || snap.ExitCode != 130 {
		t.Fatalf("snapshot = %+v ok=%v", snap, ok)
	}
}

// bg wait 预算：有界等待——指定秒数与剩余前台预算取小；预算外只查询。
func TestBGWaitBudget(t *testing.T) {
	t.Parallel()
	tasks := NewTaskTable()
	reg := commands.NewRegistry()
	_ = RegisterPlatformCommands(reg, PlatformDeps{Tasks: tasks})

	// 构造运行中任务测预算路径
	running := NewExecHandle(func() {})
	release := make(chan struct{})
	go func() { <-release; running.Finish(&ExecResult{ExitCode: 0}, nil) }()
	task, err := tasks.Adopt(running, "sleep 60", TaskMeta{Owner: "u1", Session: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := identityCtx("u1", "s1", false)
	// 无预算注入（后台语义）：指定秒数也被钳到只查询
	out, _, err := runCmdCtx(ctx, t, reg, "bg", "wait", task.ID, "30")
	if err != nil || !strings.Contains(out, "running") {
		t.Fatalf("wait without budget must not block: %q %v", out, err)
	}
	// 预算注入：wait 在预算内返回
	ctx2 := context.WithValue(ctx, waitBudgetKey{}, time.Now().Add(3*time.Second))
	start := time.Now()
	out, _, err = runCmdCtx(ctx2, t, reg, "bg", "wait", task.ID, "30")
	if err != nil || !strings.Contains(out, "running") {
		t.Fatalf("wait over budget: %q %v", out, err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("wait exceeded shared foreground budget")
	}
	close(release)
	// 禁止等待自身
	self := NewExecHandle(func() {})
	self.adopt("bg-self")
	ctx3 := context.WithValue(ctx, execHandleKey{}, self)
	if _, _, err := runCmdCtx(ctx3, t, reg, "bg", "wait", "bg-self", "1"); err == nil {
		t.Fatal("wait on self must fail")
	}
}

func TestCmdSendUserAndHosts(t *testing.T) {
	t.Parallel()
	reg := commands.NewRegistry()
	var sent string
	_ = RegisterPlatformCommands(reg, PlatformDeps{
		Tasks: NewTaskTable(),
		SendUser: func(ctx context.Context, sessionKey, msg string) error {
			sent = msg
			return nil
		},
		ListHosts: func(ctx context.Context, sessionKey string) (string, error) {
			return "| id | name |\n| h1 | mbp |\n", nil
		},
	})
	ctx := identityCtx("u1", "s1", false)
	out, _, err := runCmdCtx(ctx, t, reg, "send_user", "hello", "world")
	if err != nil || sent != "hello world" || !strings.Contains(out, "sent") {
		t.Fatalf("send_user: %q %q %v", out, sent, err)
	}
	out, _, err = runCmdCtx(ctx, t, reg, "list_hosts")
	if err != nil || !strings.Contains(out, "h1") {
		t.Fatalf("list_hosts: %q %v", out, err)
	}
}
