package exec_procs

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/veypi/aic-pod/libs/proto"
)

// spawnOrSkip 启动管道进程；沙箱后端不可用时跳过（嵌套 seatbelt 环境——
// 与 TestConfine* 同约定）。
func spawnOrSkip(t *testing.T, m *Manager, ctx context.Context, opts StartOptions) *Spawned {
	t.Helper()
	opts.NetOpen = true // 测试不经网络规则（嵌套 seatbelt 带 network 规则会 EPERM）
	sp, err := m.Spawn(ctx, opts)
	if err != nil {
		if strings.Contains(err.Error(), "no sandbox backend") {
			t.Skipf("sandbox backend unavailable (nested sandbox): %v", err)
		}
		t.Fatalf("Spawn: %v", err)
	}
	return sp
}

// Spawn 管道语义：stdout 流式读、EOF 汇合退出状态、Abort 提前终止。
// netOpen=true（测试不经网络规则；嵌套 seatbelt 带 network 规则会 EPERM，
// 见 host_sandbox.md 嵌套限制）。
func TestSpawnEcho(t *testing.T) {
	m := NewManager(time.Minute)
	sp := spawnOrSkip(t, m, context.Background(), StartOptions{FsOpen: true, NetOpen: true,
		Exec:  []string{"sh", "-c", "printf hello"},
		Level: proto.LevelRead,
	})
	body, err := io.ReadAll(sp.Body())
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "hello" {
		t.Fatalf("body = %q, want hello", body)
	}
	if err := sp.Wait(); err != nil {
		t.Fatalf("Wait after EOF should be nil: %v", err)
	}
}

// 非零退出：Body 读至 EOF 时返回带 stderr 摘要的错误（而非 io.EOF）。
func TestSpawnExitError(t *testing.T) {
	m := NewManager(time.Minute)
	sp := spawnOrSkip(t, m, context.Background(), StartOptions{FsOpen: true, NetOpen: true,
		Exec:  []string{"sh", "-c", "echo out; echo boom-err >&2; exit 3"},
		Level: proto.LevelRead,
	})
	_, err := io.ReadAll(sp.Body())
	if err == nil || !strings.Contains(err.Error(), "boom-err") {
		t.Fatalf("EOF should surface exit error with stderr tail, got: %v", err)
	}
	// Wait 幂等（第二次调用返回 nil）
	if err := sp.Wait(); err != nil {
		t.Fatalf("second Wait should be nil: %v", err)
	}
}

// Abort：提前终止（大输出场景调用方中止），Abort 幂等。
func TestSpawnAbort(t *testing.T) {
	m := NewManager(time.Minute)
	sp := spawnOrSkip(t, m, context.Background(), StartOptions{FsOpen: true, NetOpen: true,
		Exec:  []string{"sh", "-c", "yes"},
		Level: proto.LevelRead,
	})
	buf := make([]byte, 64)
	if _, err := sp.Body().Read(buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	sp.Abort()
	sp.Abort()
	if err := sp.Body().Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// ctx 取消即杀进程。
func TestSpawnCtxCancel(t *testing.T) {
	m := NewManager(time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	sp := spawnOrSkip(t, m, ctx, StartOptions{FsOpen: true, NetOpen: true,
		Exec:  []string{"sh", "-c", "sleep 30"},
		Level: proto.LevelRead,
	})
	cancel()
	done := make(chan struct{})
	go func() { sp.Abort(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ctx cancel did not kill process")
	}
}
