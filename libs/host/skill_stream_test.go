package host

// skillToolStream 桥接测试：真实 hello-service 包经
// skillrun.OpenStream → tool.Stream 验证消息语义。

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/veypi/vbox"
	"github.com/veypi/vsh/commands"

	"github.com/veypi/aic-pod/libs/skillrun"
	tool "github.com/veypi/aic-pod/protocol/hosts_tools"
)

// newStreamTestRegistry 构造隔离 skillrun Registry（无沙箱；hello 包真实构建）。
func newStreamTestRegistry(t *testing.T) *skillrun.Registry {
	t.Helper()
	root := t.TempDir()
	pkgDir := filepath.Join(root, "hello")
	binDir := filepath.Join(pkgDir, "cli", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join("..", "..", "..", "aic-skills", "hello-service", "cli", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "cli", "manifest.json"), manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(binDir, "hello-process")
	if b, err := exec.Command("go", "build", "-o", out, filepath.Join("..", "..", "..", "aic-skills", "hello", "provider", "process")).CombinedOutput(); err != nil {
		t.Fatalf("build hello-process: %v\n%s", err, b)
	}
	outSvc := filepath.Join(binDir, "hello-service")
	if b, err := exec.Command("go", "build", "-o", outSvc, filepath.Join("..", "..", "..", "aic-skills", "hello-service", "provider", "service")).CombinedOutput(); err != nil {
		t.Fatalf("build hello-service: %v\n%s", err, b)
	}
	// macOS unix socket 104 字符上限：run 目录用短路径。
	runDir, err := os.MkdirTemp("/tmp", "skr-host-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(runDir) })
	reg := commands.NewRegistry()
	m := vbox.NewManager()
	m.SetNoSandbox(true)
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	r, err := skillrun.New(skillrun.Deps{
		SkillsDir: filepath.Join(root, "skills"),
		RunDir:    runDir,
		Manager:   m,
		Workdir:   func(s string) string { return s },
		Registry:  reg,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	if _, err := installTestPackage(t, r, pkgDir); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSkillToolStreamEcho(t *testing.T) {
	r := newStreamTestRegistry(t)
	s, err := r.OpenStream(context.Background(), "hello", "echo")
	if err != nil {
		t.Fatal(err)
	}
	st := newSkillToolStream(s)
	defer st.Close()
	ctx := context.Background()
	// 消息边界保持：两条 Send = 两条 Recv（含二进制与 0 字节）
	if err := st.Send(ctx, []byte("msg\x00one")); err != nil {
		t.Fatal(err)
	}
	if err := st.Send(ctx, []byte("second")); err != nil {
		t.Fatal(err)
	}
	b1, err := st.Recv(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := st.Recv(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(b1) != "msg\x00one" || string(b2) != "second" {
		t.Fatalf("messages = %q, %q", b1, b2)
	}
}

func TestSkillToolStreamCloseUnblocksRecv(t *testing.T) {
	r := newStreamTestRegistry(t)
	s, err := r.OpenStream(context.Background(), "hello", "echo")
	if err != nil {
		t.Fatal(err)
	}
	st := newSkillToolStream(s)
	done := make(chan error, 1)
	go func() {
		_, err := st.Recv(context.Background())
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	st.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Recv after Close must return error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not unblock Recv")
	}
	// Close 幂等
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSkillToolStreamRecvCtxCancel(t *testing.T) {
	r := newStreamTestRegistry(t)
	s, err := r.OpenStream(context.Background(), "hello", "echo")
	if err != nil {
		t.Fatal(err)
	}
	st := newSkillToolStream(s)
	defer st.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := st.Recv(ctx); err == nil {
		t.Fatal("Recv with no frames must honor ctx deadline")
	}
	// ctx 取消只放弃本次等待——流仍可续读
	if err := st.Send(context.Background(), []byte("after")); err != nil {
		t.Fatal(err)
	}
	b, err := st.Recv(context.Background())
	if err != nil || string(b) != "after" {
		t.Fatalf("Recv after ctx cancel = %q, %v", b, err)
	}
}

// buildBrowserPkg 装配 browser 源包并编译真实 service provider。
func buildBrowserPkg(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	pkgDir := filepath.Join(root, "browser")
	binDir := filepath.Join(pkgDir, "cli", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join("..", "..", "..", "aic-skills", "browser")
	manifest, err := os.ReadFile(filepath.Join(src, "cli", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "cli", "manifest.json"), manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{"browser-service": "service"} {
		out := filepath.Join(binDir, name)
		if b, err := exec.Command("go", "build", "-o", out, filepath.Join(src, "provider", dir)).CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", name, err, b)
		}
	}
	return pkgDir
}

// TestOpenToolStreamFullNameEndpoint 验证 <package>.<stream> 解析：
// browser.page.frames → browser 包的 page.frames 流，stream.open 负载
// 原样透传到 svc（不存在的页面 → svc not_found 错误经桥接回来）；未知端点
// 显式失败。
func TestOpenToolStreamFullNameEndpoint(t *testing.T) {
	// 编译夹具使用开发者的工具链环境；只有应用运行隔离 HOME。
	r := newStreamTestRegistry(t)
	if _, err := installTestPackage(t, r, buildBrowserPkg(t)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	c := New(Options{Key: "host_1.1.secret.owner", WorkDir: t.TempDir(), NoSandbox: true})
	defer c.Close()
	c.skills = r
	caller := tool.Caller{Subject: "owner", ConnectionID: "rtc:test", Origin: "sess", AllowStreams: true, ExpiresAt: time.Now().Add(time.Minute)}
	ctx := context.Background()
	st, err := c.OpenToolStream(ctx, caller, "browser.page.frames", json.RawMessage(`{"page_id":"p_nope"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Recv(ctx); err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("expected not_found from browser svc, got %v", err)
	}
	if _, err := c.OpenToolStream(ctx, caller, "page.bogus", nil); err == nil || !strings.Contains(err.Error(), "Unknown stream endpoint") {
		t.Fatalf("unknown endpoint: %v", err)
	}
}

func installTestPackage(t *testing.T, r *skillrun.Registry, dir string) (*skillrun.Package, error) {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		w, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(dir)
	return r.InstallZip(context.Background(), b.Bytes(), &skillrun.FetchMeta{Name: name, Kind: "private", ID: name})
}
