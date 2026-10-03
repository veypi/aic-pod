package skillrun

// browser 根命令直接调用驻留 service；status 不启动 Chrome。

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildBrowserPkg 装配 browser 源包到临时目录并构建真实 provider 二进制
// （与 hello 同约定：仓库为源，产物落 cli/bin/）。
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

// TestBrowserPackageStatusEndToEnd 验证 service 默认 provider 的命令与错误透传。
func TestBrowserPackageStatusEndToEnd(t *testing.T) {
	r, reg := newTestRegistry(t, true)
	if _, err := installTestPackage(t, r, buildBrowserPkg(t)); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := invoke(t, reg, "browser", []string{"status", "--json"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, `"state"`) || !strings.Contains(stdout, `"viewport"`) {
		t.Fatalf("status output: %s", stdout)
	}
	// svc 懒启动登记（确保机制生效的事实断言）。
	up := packageService(r, "browser") != nil
	if !up {
		t.Fatal("svc provider not started by package command invocation")
	}
	// --help 同链路（svc 侧命令表）。
	stdout, _, err = invoke(t, reg, "browser", []string{"--help"}, "")
	if err != nil || !strings.Contains(stdout, "page.create") {
		t.Fatalf("help: %v %s", err, stdout)
	}
	// 未知子命令：exit 2 + usage（CLI 契约不变）。
	_, stderr, err := invoke(t, reg, "browser", []string{"nope"}, "")
	if err == nil || !strings.Contains(stderr, "unknown subcommand") {
		t.Fatalf("unknown subcommand: %v %s", err, stderr)
	}
}

// TestResolveStreamEndpoint 只接受包名限定端点，不扫描裸流名。
func TestResolveStreamEndpoint(t *testing.T) {
	r, _ := newTestRegistry(t, true)
	if _, err := installTestPackage(t, r, buildHelloServicePkg(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := installTestPackage(t, r, buildBrowserPkg(t)); err != nil {
		t.Fatal(err)
	}
	for endpoint, want := range map[string][2]string{
		"hello.echo":          {"hello", "echo"},
		"browser.page.frames": {"browser", "page.frames"},
		"browser.page.input":  {"browser", "page.input"},
	} {
		pkg, stream, ok := r.ResolveStreamEndpoint(endpoint)
		if !ok || pkg != want[0] || stream != want[1] {
			t.Fatalf("ResolveStreamEndpoint(%q) = %q, %q, %v", endpoint, pkg, stream, ok)
		}
	}
	for _, endpoint := range []string{"page.bogus", "page.frames", "page.input", "nope", "hello.page.frames"} {
		if _, _, ok := r.ResolveStreamEndpoint(endpoint); ok {
			t.Fatalf("ResolveStreamEndpoint(%q) unexpectedly resolved", endpoint)
		}
	}
}

// TestBrowserStreamOpenArgs stream.open 负载透传：page.frames 带 PageArgs 打开
// 不存在的页面 → svc 侧参数校验/查找错误经 error 帧回来（证明 args 到达 svc）。
func TestBrowserStreamOpenArgs(t *testing.T) {
	r, _ := newTestRegistry(t, true)
	if _, err := installTestPackage(t, r, buildBrowserPkg(t)); err != nil {
		t.Fatal(err)
	}
	s, err := r.OpenStream(context.Background(), "browser", "page.frames", []byte(`{"page_id":"p_nope"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.ReadFrame(); err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("expected not_found from svc, got %v", err)
	}
	// 缺 page_id：svc 参数校验。
	s2, err := r.OpenStream(context.Background(), "browser", "page.input", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := s2.ReadFrame(); err == nil || !strings.Contains(err.Error(), "page_id") {
		t.Fatalf("expected page_id validation error from svc, got %v", err)
	}
}
