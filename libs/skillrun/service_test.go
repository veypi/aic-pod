package skillrun

import (
	"archive/zip"
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/veypi/aic-skills/sdk/go/skillproc"
	"github.com/veypi/vsh/commands"
)

// buildSvcDefaultPkg 装配 单 service 的包变体（service 根命令调用
// 路径——browser 类包的真实形态；与 hello 共用二进制）。
func buildSvcDefaultPkg(t *testing.T) string {
	t.Helper()
	pkgDir := buildHelloServicePkg(t) // {root}/hello
	svcDir := filepath.Join(filepath.Dir(pkgDir), "hellosvc")
	if err := os.Rename(pkgDir, svcDir); err != nil {
		t.Fatal(err)
	}
	manifest := `{"kind":"service","entry":"cli/bin/hello-service","streams":["echo"]}`
	if err := os.WriteFile(filepath.Join(svcDir, "cli", "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return svcDir
}

// TestOpenStreamDisabled 禁用包 stream 显式失败（与根命令同语义）。
func TestOpenStreamDisabled(t *testing.T) {
	r, _ := newTestRegistry(t, true)
	if _, err := installTestPackage(t, r, buildHelloServicePkg(t)); err != nil {
		t.Fatal(err)
	}
	if err := r.SetDisabled("hello", true); err != nil {
		t.Fatal(err)
	}
	if _, err := r.OpenStream(context.Background(), "hello", "echo"); err == nil {
		t.Fatal("disabled package stream must fail explicitly")
	}
}

// TestOpenStreamNonCLI 非 CLI 资源包（Manifest=nil）无 stream 声明。
func TestOpenStreamNonCLI(t *testing.T) {
	r, _ := newTestRegistryFetch(t, true, func(ctx context.Context, ref, ver string) ([]byte, *FetchMeta, error) {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		w, _ := zw.Create("scripts/run.sh")
		_, _ = w.Write([]byte("#!/bin/sh\n"))
		_ = zw.Close()
		return buf.Bytes(), &FetchMeta{Name: "assets", Kind: "public", ID: "uuid-9"}, nil
	})
	if _, err := r.Download(context.Background(), "uuid-9", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.OpenStream(context.Background(), "assets", "echo"); err == nil {
		t.Fatal("non-CLI package stream must fail explicitly")
	}
}

// TestReadFrameBoundary 帧边界保持：两次 Write = 两次 ReadFrame（消息语义，
// RTC 桥接依赖——Read 字节流会展平）。
func TestReadFrameBoundary(t *testing.T) {
	r, _ := newTestRegistry(t, true)
	if _, err := installTestPackage(t, r, buildHelloServicePkg(t)); err != nil {
		t.Fatal(err)
	}
	s, err := r.OpenStream(context.Background(), "hello", "echo")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Write([]byte("one")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write([]byte("two-two")); err != nil {
		t.Fatal(err)
	}
	f1, err := s.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	f2, err := s.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if string(f1) != "one" || string(f2) != "two-two" {
		t.Fatalf("frames = %q, %q", f1, f2)
	}
}

// TestServiceLazyStartAndStream 懒启动 + 独立于 bg + stream.open 二进制回显 +
// 实例复用（第二次 OpenStream 不重拉）。
func TestServiceLazyStartAndStream(t *testing.T) {
	r, _, tasks := newTestRegistryTasks(t, true)
	if _, err := installTestPackage(t, r, buildHelloServicePkg(t)); err != nil {
		t.Fatal(err)
	}
	s, err := r.OpenStream(context.Background(), "hello", "echo")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if len(tasks.List("", "")) != 0 {
		t.Fatal("skill service must not occupy bg tasks")
	}
	// 二进制回显（含 0 字节与大数据帧）
	payload := append([]byte("bin\x00\x01"), bytes.Repeat([]byte("z"), 70000)...)
	if _, err := s.Write(payload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 0, len(payload))
	buf := make([]byte, 8192)
	for len(got) < len(payload) {
		n, err := s.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, buf[:n]...)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("echo payload mismatch: %d != %d bytes", len(got), len(payload))
	}
	// 实例复用
	insts := packageService(r, "hello")
	second, err := r.OpenStream(context.Background(), "hello", "echo")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if packageService(r, "hello") != insts {
		t.Fatal("second OpenStream must reuse the running instance")
	}
}

// TestServiceInvokeRootCommand service provider 的根命令调用（browser
// 类形态）：argv 回显 / stdin 负载 / exit code 透传。
func TestServiceInvokeRootCommand(t *testing.T) {
	r, reg := newTestRegistry(t, true)
	if _, err := installTestPackage(t, r, buildSvcDefaultPkg(t)); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := invoke(t, reg, "hellosvc", []string{"argv", "x", "y"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "x\ny\n" {
		t.Fatalf("argv stdout = %q", stdout)
	}
	stdout, _, err = invoke(t, reg, "hellosvc", []string{"pipe"}, "pay\x00load")
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "pay\x00load" {
		t.Fatalf("pipe stdout = %q", stdout)
	}
	_, _, err = invoke(t, reg, "hellosvc", []string{"exit", "7"}, "")
	if ee, ok := err.(*commands.ExitError); !ok || ee.Code != 7 {
		t.Fatalf("err = %v, want ExitError(7)", err)
	}
}

// TestServiceCancel sleep 调用经关闭连接中断（exit 124）。
func TestServiceCancel(t *testing.T) {
	r, reg := newTestRegistry(t, true)
	if _, err := installTestPackage(t, r, buildSvcDefaultPkg(t)); err != nil {
		t.Fatal(err)
	}
	cmd, _ := reg.Lookup("hellosvc")
	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr bytes.Buffer
	inv := commands.NewInvocation(&commands.InvocationOptions{
		Args:   []string{"sleep", "30"},
		Cwd:    t.TempDir(),
		Stdout: &stdout,
		Stderr: &stderr,
	})
	done := make(chan error, 1)
	go func() { done <- cmd.Run(ctx, inv) }()
	time.Sleep(500 * time.Millisecond)
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled service invoke must return error")
		}
		if ee, ok := err.(*commands.ExitError); !ok || ee.Code != 124 {
			t.Fatalf("err = %v, want ExitError(124)", err)
		}
		if time.Since(start) > 5*time.Second {
			t.Fatal("cancel propagation too slow")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancel did not reach service provider")
	}
}

// TestServiceCrashRestart 崩溃后下次调用重拉（无 supervisor，重拉即恢复）。
func TestServiceCrashRestart(t *testing.T) {
	r, _ := newTestRegistry(t, true)
	if _, err := installTestPackage(t, r, buildHelloServicePkg(t)); err != nil {
		t.Fatal(err)
	}
	s1, err := r.OpenStream(context.Background(), "hello", "echo")
	if err != nil {
		t.Fatal(err)
	}
	// 杀进程（模拟崩溃）
	inst := packageService(r, "hello")
	if inst == nil {
		t.Fatal("service instance missing")
	}
	inst.cancel()
	select {
	case <-inst.done:
	case <-time.After(5 * time.Second):
		t.Fatal("service did not exit after kill")
	}
	s1.Close()
	// 重拉
	s2, err := r.OpenStream(context.Background(), "hello", "echo")
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := s2.Write([]byte("again")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	n, err := s2.Read(buf)
	if err != nil || string(buf[:n]) != "again" {
		t.Fatalf("restarted echo = %q, %v", buf[:n], err)
	}
}

// TestUninstallKillsService 卸载 = 解注册 + 停止并等待 provider + 删目录。
func TestUninstallKillsService(t *testing.T) {
	r, _ := newTestRegistry(t, true)
	if _, err := installTestPackage(t, r, buildHelloServicePkg(t)); err != nil {
		t.Fatal(err)
	}
	s, err := r.OpenStream(context.Background(), "hello", "echo")
	if err != nil {
		t.Fatal(err)
	}
	inst := packageService(r, "hello")
	if err := r.Uninstall("hello"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-inst.done:
	case <-time.After(5 * time.Second):
		t.Fatal("uninstall must kill resident provider")
	}
	s.Close()
	if packageService(r, "hello") != nil {
		t.Fatal("service instance must be dropped on uninstall")
	}
}

// A non-cooperating service must not keep a canceled caller blocked in Recv.
func TestServiceCwdAndDisconnectedCancellation(t *testing.T) {
	r, _ := newTestRegistry(t, true)
	socket := filepath.Join(r.deps.RunDir, "probe.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	type received struct {
		header skillproc.Header
		data   []byte
	}
	receivedCh := make(chan received, 1)
	go func() {
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			conn := skillproc.NewConn(raw)
			h, data, err := conn.Recv()
			if err != nil {
				conn.Close()
				continue
			} // readiness probe
			receivedCh <- received{h, data}
			_, _, _ = conn.Recv() // no reply; wait for client disconnect
			conn.Close()
			return
		}
	}()
	ready := make(chan struct{})
	close(ready)
	pkg := &Package{Name: "probe", Manifest: &Manifest{Kind: KindService}, service: &serviceInst{socket: socket, done: make(chan struct{}), ready: ready}}
	r.deps.Workdir = func(cwd string) string {
		if cwd != "/c/work" {
			t.Errorf("cwd=%q", cwd)
		}
		return `C:\work`
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out, errs bytes.Buffer
	env := map[string]string{"DEMO": "value with spaces=a:b", "PATH": "/c/tools:/c/bin", "HOME": "/c/home", "TMPDIR": "/tmp", "EMPTY": ""}
	wantEnv := map[string]string{"DEMO": "value with spaces=a:b", "PATH": "/c/tools:/c/bin", "HOME": "/c/home", "TMPDIR": "/tmp", "EMPTY": ""}
	if runtime.GOOS == "windows" {
		wantEnv["PATH"], wantEnv["HOME"], wantEnv["TMPDIR"] = `C:\tools;C:\bin`, `C:\home`, os.TempDir()
	}
	inv := &commands.Invocation{Cwd: "/c/work", Env: env, Args: []string{"arg with spaces"}, Stdin: bytes.NewReader([]byte{'a', 0, 'b'}), Stdout: &out, Stderr: &errs}
	done := make(chan error, 1)
	go func() {
		done <- r.invokeService(ctx, inv, pkg)
	}()
	select {
	case got := <-receivedCh:
		if !reflect.DeepEqual(got.header.Env, wantEnv) {
			t.Fatalf("invoke environment=%q, want %q", got.header.Env, wantEnv)
		}
		if got.header.Cwd != `C:\work` || len(got.header.Argv) != 1 || got.header.Argv[0] != "arg with spaces" || !bytes.Equal(got.data, []byte{'a', 0, 'b'}) {
			t.Fatalf("invoke lost cwd/argv/stdin: %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("invoke did not arrive")
	}
	cancel()
	select {
	case err := <-done:
		if ee, ok := err.(*commands.ExitError); !ok || ee.Code != 124 {
			t.Fatalf("cancel result: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation blocked on service response")
	}
}

func buildHelloServicePkg(t *testing.T) string {
	dir := buildHelloPkg(t)
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "aic-skills", "hello-service", "cli", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cli", "manifest.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}
