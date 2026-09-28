package execwait

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	vshglue "github.com/veypi/aic-pod/libs/vsh"
	"github.com/veypi/vigo/contrib/ufs"
	gbfs "github.com/veypi/vsh/fs"
)

// newTestEngine cloud 形态 Engine：UFS localFS backing + jail /u/u1，无规则表。
func newTestEngine(t *testing.T) *vshglue.Engine {
	t.Helper()
	backing, err := ufs.NewLocalFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e, err := vshglue.NewEngine(vshglue.EngineConfig{
		BaseEnv: map[string]string{"HOME": "/u/u1", "PATH": "/usr/bin:/bin"},
		NewSessionFS: func(key string) (gbfs.FileSystem, string, error) {
			fsys, err := vshglue.NewCloudFS(vshglue.CloudFSConfig{UserRoot: "/u/u1", Backing: backing})
			return fsys, "/u/u1", err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// blockHandle 构造阻塞中的执行句柄（Adopt 测试件）。
func blockHandle() (*vshglue.ExecHandle, chan struct{}) {
	release := make(chan struct{})
	h := vshglue.NewExecHandle(func() {})
	go func() { <-release; h.Finish(&vshglue.ExecResult{ExitCode: 0}, nil) }()
	return h, release
}

// TestForegroundNoBGRecord 前台完成不产生 bg 记录（§2.4）。
func TestForegroundNoBGRecord(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t)
	out := Execute(context.Background(), e, vshglue.ExecRequest{
		SessionKey: "s1", Owner: "u:t1", Script: "echo hi",
	}, 5*time.Second, vshglue.TaskMeta{Owner: "u:t1", Session: "s1"}, true)
	if out.Err != nil || out.Background || out.Result.ExitCode != 0 {
		t.Fatalf("out = %+v", out)
	}
	if strings.TrimSpace(out.Result.Stdout) != "hi" {
		t.Fatalf("stdout = %q", out.Result.Stdout)
	}
	if got := e.Tasks.List("u:t1", "s1"); len(got) != 0 {
		t.Fatalf("前台完成不应留 bg 记录: %v", got)
	}
}

// TestNoAdoptRTC RTC 直连语义（adoptOnTimeout=false）：等待超时返回
// ErrWaitElapsed，不产生 bg 记录；执行继续并正常完成（bg 机制只服务 AI 通道）。
func TestNoAdoptRTC(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t)
	h := vshglue.NewExecHandle(nil)
	out := Execute(context.Background(), e, vshglue.ExecRequest{
		SessionKey: "s1", Owner: "u:t1", Script: "sleep 0.3; echo late", Handle: h,
	}, 50*time.Millisecond, vshglue.TaskMeta{Owner: "u:t1", Session: "s1"}, false)
	if !errors.Is(out.Err, ErrWaitElapsed) || out.Background || out.Result != nil {
		t.Fatalf("out = %+v", out)
	}
	if got := e.Tasks.List("u:t1", "s1"); len(got) != 0 {
		t.Fatalf("RTC 超时不应产生 bg 记录: %v", got)
	}
	// 执行继续并完成（取消只能经句柄/cancel——本例等自然完成）。
	select {
	case <-h.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("execution did not continue to completion")
	}
	res, _ := h.Result()
	if res == nil || strings.TrimSpace(res.Stdout) != "late" {
		t.Fatalf("res = %+v", res)
	}
	if got := e.Tasks.List("u:t1", "s1"); len(got) != 0 {
		t.Fatalf("完成后也不应补 bg 记录: %v", got)
	}
}

// TestTimeoutAdoptsOnce 等待超时登记后台一次：不重启、不换句柄；
// bg wait 可等到完成；退出码透传。
func TestTimeoutAdoptsOnce(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t)
	out := Execute(context.Background(), e, vshglue.ExecRequest{
		SessionKey: "s1", Owner: "u:t1", Script: "sleep 0.3; echo late",
	}, 50*time.Millisecond, vshglue.TaskMeta{Owner: "u:t1", Session: "s1", LogOut: "/log/o", LogErr: "/log/e"}, true)
	if out.Err != nil || !out.Background {
		t.Fatalf("out = %+v", out)
	}
	id := out.Task.ID
	if !strings.HasPrefix(id, "bg-") {
		t.Fatalf("task id = %q", id)
	}
	if out.Task.LogOut != "/log/o" || out.Task.LogErr != "/log/e" {
		t.Fatalf("log paths = %+v", out.Task)
	}
	task, err := e.Tasks.Wait(context.Background(), id, 5*time.Second, "u:t1", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != "done" || task.ExitCode != 0 {
		t.Fatalf("task = %+v", task)
	}
	if _, ok := e.Tasks.Get(id, "u:t1", "s2"); ok {
		t.Fatal("cross-session task visible")
	}
}

// TestContextCancelStopsWaitingOnly 调用方 ctx 结束（传输断连/预算到期）：
// 结束等待、不取消执行、不登记后台（§2.6：断线不是取消）——执行
// goroutine 继续跑完写结果，返回上下文取消类错误。
func TestContextCancelStopsWaitingOnly(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t)
	h := vshglue.NewExecHandle(nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *Outcome, 1)
	go func() {
		done <- Execute(ctx, e, vshglue.ExecRequest{
			SessionKey: "s1", Owner: "u:t1", Script: "sleep 0.3; echo late", Handle: h,
		}, 30*time.Second, vshglue.TaskMeta{Owner: "u:t1", Session: "s1"}, true)
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case out := <-done:
		if !errors.Is(out.Err, context.Canceled) || out.Background || out.Result != nil {
			t.Fatalf("out = %+v", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ctx cancel did not end the foreground wait")
	}
	if got := e.Tasks.List("u:t1", "s1"); len(got) != 0 {
		t.Fatalf("ctx 取消不应登记后台: %v", got)
	}
	// 执行不被取消：跑完并写入结果。
	select {
	case <-h.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("execution was cancelled by caller ctx")
	}
	res, _ := h.Result()
	if res == nil || strings.TrimSpace(res.Stdout) != "late" {
		t.Fatalf("res = %+v", res)
	}
	if got := e.Tasks.List("u:t1", "s1"); len(got) != 0 {
		t.Fatalf("完成后也不应补 bg 记录: %v", got)
	}
}

// TestCapacityCancel 转后台容量不足：取消本次执行并返回容量错误，
// 不让未登记的执行继续运行（§2.4）。
func TestCapacityCancel(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t)
	e.Tasks = vshglue.NewTaskTableWithCaps(1, 1)
	// 占满唯一名额
	h, release := blockHandle()
	defer close(release)
	if _, err := e.Tasks.Adopt(h, "occupant", vshglue.TaskMeta{Owner: "u:t1", Session: "s1"}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	out := Execute(context.Background(), e, vshglue.ExecRequest{
		SessionKey: "s1", Owner: "u:t1", Script: "sleep 30",
	}, 50*time.Millisecond, vshglue.TaskMeta{Owner: "u:t1", Session: "s1"}, true)
	if out.Err == nil || !strings.Contains(out.Err.Error(), "task table full") {
		t.Fatalf("out.Err = %v", out.Err)
	}
	if !errors.Is(out.Err, ErrCapacity) {
		t.Fatalf("容量错误必须携带 ErrCapacity 哨兵: %v", out.Err)
	}
	if out.Background {
		t.Fatal("容量不足不应登记后台")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("未登记的执行未被取消（sleep 30 跑满）")
	}
}
