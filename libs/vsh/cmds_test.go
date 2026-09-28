package vsh

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/veypi/vsh/commands"
)

func runCmd(t *testing.T, reg *commands.Registry, name string, args ...string) (string, string, error) {
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
	err := commands.RunCommand(context.Background(), cmd, inv)
	return out.String(), errBuf.String(), err
}

func TestPlatformCommandsRegistered(t *testing.T) {
	t.Parallel()
	reg := commands.NewRegistry()
	if err := RegisterPlatformCommands(reg, PlatformDeps{Tasks: NewTaskTable()}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"commands", "bg", "grant", "list_hosts", "send_user"} {
		if _, ok := reg.Lookup(name); !ok {
			t.Fatalf("missing %q", name)
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
	if !strings.Contains(out, "grant") || !strings.Contains(out, "bg") {
		t.Fatalf("commands out = %q", out)
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
			return "已提交审批", nil
		},
	}
	_ = RegisterPlatformCommands(reg, deps)
	out, _, err := runCmd(t, reg, "grant", "fs", "/u/u1/docs")
	if err != nil {
		t.Fatal(err)
	}
	if gotDomain != "fs" || gotTarget != "/u/u1/docs" || gotPermanent || !strings.Contains(out, "已提交审批") {
		t.Fatalf("grant: %q %q %v %q", gotDomain, gotTarget, gotPermanent, out)
	}
	// ssh 域 + --permanent（任意位置）透传。
	if _, _, err = runCmd(t, reg, "grant", "ssh", "example.com:22", "--permanent"); err != nil {
		t.Fatal(err)
	}
	if gotDomain != "ssh" || gotTarget != "example.com:22" || !gotPermanent {
		t.Fatalf("grant ssh --permanent: %q %q %v", gotDomain, gotTarget, gotPermanent)
	}
	if _, _, err = runCmd(t, reg, "grant", "--permanent", "net", "example.com:443"); err != nil {
		t.Fatal(err)
	}
	if gotDomain != "net" || !gotPermanent {
		t.Fatalf("grant --permanent net: %q %v", gotDomain, gotPermanent)
	}
	// 未知域报错。
	_, _, err = runCmd(t, reg, "grant", "root", "/")
	if err == nil {
		t.Fatal("unknown domain should fail")
	}
	// help 自答。
	out, _, err = runCmd(t, reg, "grant", "--help")
	if err != nil || !strings.Contains(out, "grant fs") {
		t.Fatalf("help: %q %v", out, err)
	}
}

func TestCmdBGClosedLoop(t *testing.T) {
	t.Parallel()
	tasks := NewTaskTable()
	reg := commands.NewRegistry()
	deps := PlatformDeps{
		Tasks: tasks,
		RunBG: func(ctx context.Context, sessionKey, script, workdir, logPath string, log io.Writer) (int, error) {
			if _, err := log.Write([]byte("bg-output\n")); err != nil {
				return 1, err
			}
			return 0, nil
		},
	}
	_ = RegisterPlatformCommands(reg, deps)
	out, _, err := runCmd(t, reg, "bg", "run", "echo hi")
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(out)
	out, _, err = runCmd(t, reg, "bg", "wait", id, "5")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "bg-output") || !strings.Contains(out, "done") {
		t.Fatalf("wait out = %q", out)
	}
	out, _, _ = runCmd(t, reg, "bg", "list")
	if !strings.Contains(out, id) {
		t.Fatalf("list = %q", out)
	}
	// kill 不存在任务报错。
	_, _, err = runCmd(t, reg, "bg", "kill", "bg-999")
	if err == nil {
		t.Fatal("kill unknown should fail")
	}
}

func TestTaskTableWallClock(t *testing.T) {
	t.Parallel()
	tasks := NewTaskTable()
	// 直接压任务表墙钟（30min 不可测——验证 run ctx 带 deadline 即可）。
	task, err := tasks.Start("check-deadline", "", "o1", func(ctx context.Context, log io.Writer) (int, error) {
		dl, ok := ctx.Deadline()
		if !ok {
			return 1, nil
		}
		if time.Until(dl) > BackgroundWallClock || time.Until(dl) < BackgroundWallClock-time.Minute {
			return 2, nil
		}
		return 0, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := tasks.Wait(context.Background(), task.ID, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExitCode != 0 || got.Status != "done" {
		t.Fatalf("wall clock ctx wrong: %+v", got)
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
	out, _, err := runCmd(t, reg, "send_user", "hello", "world")
	if err != nil || sent != "hello world" || !strings.Contains(out, "sent") {
		t.Fatalf("send_user: %q %q %v", out, sent, err)
	}
	out, _, err = runCmd(t, reg, "list_hosts")
	if err != nil || !strings.Contains(out, "h1") {
		t.Fatalf("list_hosts: %q %v", out, err)
	}
}
