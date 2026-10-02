package skillrun

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/veypi/vbox"
	"github.com/veypi/vsh/commands"

	vshglue "github.com/veypi/aic-pod/libs/vsh"
)

// buildHelloPkg 装配 hello 源包到临时目录并构建真实 provider 二进制
// （真实外部进程——不 mock 包内容）。
func buildHelloPkg(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	pkgDir := filepath.Join(root, "hello")
	binDir := filepath.Join(pkgDir, "cli", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join("..", "..", "..", "aic-skills", "hello", "cli", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "cli", "manifest.json"), manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(binDir, "hello-process")
	build := exec.Command("go", "build", "-o", out, filepath.Join("..", "..", "..", "aic-skills", "hello", "provider", "process"))
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build hello-process: %v\n%s", err, b)
	}
	outSvc := filepath.Join(binDir, "hello-service")
	buildSvc := exec.Command("go", "build", "-o", outSvc, filepath.Join("..", "..", "..", "aic-skills", "hello", "provider", "service"))
	if b, err := buildSvc.CombinedOutput(); err != nil {
		t.Fatalf("build hello-service: %v\n%s", err, b)
	}
	return pkgDir
}

// newTestRegistry 构造隔离 Registry：固定 vsh 注册表 + 真实 vbox Manager；
// 沙箱策略 = 临时目录可写（无 deny——测试策略场景）。noSandbox=false 需要
// 真实沙箱后端（嵌套沙箱环境不可用，用 sandboxUsable 预探）。
func newTestRegistry(t *testing.T, noSandbox bool) (*Registry, *commands.Registry) {
	t.Helper()
	r, reg, _ := newTestRegistryTasks(t, noSandbox)
	return r, reg
}

// newTestRegistryTasks 同 newTestRegistry，额外返回 bg 任务表（service 登记断言）。
func newTestRegistryTasks(t *testing.T, noSandbox bool) (*Registry, *commands.Registry, *vshglue.TaskTable) {
	t.Helper()
	reg := commands.NewRegistry()
	m := vbox.NewManager(5 * time.Minute)
	m.SetNoSandbox(noSandbox)
	tasks := vshglue.NewTaskTableWithCaps(8, 4)
	r, err := New(Deps{
		SkillsDir: filepath.Join(t.TempDir(), "skills"),
		RunDir:    shortRunDir(t),
		Manager:   m,
		Policy: func(ctx context.Context, workdir, name string) vshglue.NativePolicy {
			// 只授当次工作区——边界测试的包目录（t.TempDir 树下）必须留在白
			// 名单外；os.TempDir() 若同授会把 t.TempDir 全部覆盖，断言失效。
			return vshglue.NativePolicy{WriteRoots: canonicalRoots(workdir)}
		},
		Workdir:  func(invCwd string) string { return invCwd },
		Registry: func() (*commands.Registry, error) { return reg, nil },
		Tasks:    func() (*vshglue.TaskTable, error) { return tasks, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return r, reg, tasks
}

// shortRunDir 短路径 run 目录：macOS unix socket 有 104 字符路径上限，
// t.TempDir()（/var/folders/.../TestLongNamexxx/001）下套 socket 名必超。
func shortRunDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "skr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// canonicalRoots 展开符号链接前缀（darwin /var→/private/var）——seatbelt
// 规则匹配内核解析后的真实路径，与生产 fsauth.WriteRootsFor 的 canonical
// 根同口径；不展开则白名单形同虚设（写 EPERM）。
func canonicalRoots(roots ...string) []string {
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		if real, err := filepath.EvalSymlinks(r); err == nil {
			r = real
		}
		out = append(out, r)
	}
	return out
}

// sandboxUsable 预探沙箱后端（agent 沙箱内跑 go test 时嵌套沙箱不可用——
// 真机直跑测全套，嵌套环境边界测试跳过）。
func sandboxUsable() bool {
	m := vbox.NewManager(time.Minute)
	_, err := m.RunProcess(context.Background(), vbox.StartOptions{Exec: []string{"/usr/bin/true"}}, nil, io.Discard, io.Discard)
	return err == nil || !strings.Contains(err.Error(), "no sandbox backend")
}

func invoke(t *testing.T, reg *commands.Registry, name string, args []string, stdin string) (string, string, error) {
	t.Helper()
	return invokeAt(t, reg, t.TempDir(), name, args, stdin)
}

func invokeAt(t *testing.T, reg *commands.Registry, cwd, name string, args []string, stdin string) (string, string, error) {
	t.Helper()
	cmd, ok := reg.Lookup(name)
	if !ok {
		t.Fatalf("Lookup(%q) miss", name)
	}
	var stdout, stderr bytes.Buffer
	inv := commands.NewInvocation(&commands.InvocationOptions{
		Args:   args,
		Cwd:    cwd,
		Stdin:  strings.NewReader(stdin),
		Stdout: &stdout,
		Stderr: &stderr,
	})
	err := cmd.Run(context.Background(), inv)
	return stdout.String(), stderr.String(), err
}

func TestInstallRegisterRun(t *testing.T) {
	r, reg := newTestRegistry(t, true)
	pkg, err := r.Install(buildHelloPkg(t))
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Name != "hello" {
		t.Fatalf("pkg.Name = %q, want hello", pkg.Name)
	}
	if !r.IsPackageCommand("hello") {
		t.Fatal("IsPackageCommand(hello) = false")
	}
	// 无参调用
	stdout, _, err := invoke(t, reg, "hello", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(stdout) != "hello from aic skill" {
		t.Fatalf("stdout = %q", stdout)
	}
	// argv 透传
	stdout, _, err = invoke(t, reg, "hello", []string{"argv", "a", "b  c", "中文"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "a\nb  c\n中文\n" {
		t.Fatalf("argv stdout = %q", stdout)
	}
	// stdin 二进制透传
	stdout, _, err = invoke(t, reg, "hello", []string{"pipe"}, "bin\x00\x01data")
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "bin\x00\x01data" {
		t.Fatalf("pipe stdout = %q", stdout)
	}
	// 未知子命令 → exit 2
	_, _, err = invoke(t, reg, "hello", []string{"nope"}, "")
	if ee, ok := err.(*commands.ExitError); !ok || ee.Code != 2 {
		t.Fatalf("err = %v, want ExitError(2)", err)
	}
}

func TestInstallConflictRejected(t *testing.T) {
	r, reg := newTestRegistry(t, true)
	// 与已注册命令（内建/保留名同口径）冲突 → 显式拒绝
	if err := reg.Register(commands.DefineCommand("hello", nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Install(buildHelloPkg(t)); err == nil {
		t.Fatal("install over registered name must be rejected")
	}
	// 异源同名 → 显式拒绝不覆盖（public 包 vs local 目录）
	r2, _ := newTestRegistryFetch(t, true, fakeFetch(t, "uuid-1", "1.0.0", nil))
	if _, err := r2.Download(context.Background(), "uuid-1", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r2.Install(buildHelloPkg(t)); err == nil {
		t.Fatal("local install over public package must be rejected")
	}
}

// TestInstallSameSourceUpdate 同源同名 = 更新：停 provider、替换目录、重注册，
// 禁用态保留（docs/skill.md §9.2）。
func TestInstallSameSourceUpdate(t *testing.T) {
	r, reg := newTestRegistry(t, true)
	src := buildHelloPkg(t)
	pkg, err := r.Install(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetDisabled("hello", true); err != nil {
		t.Fatal(err)
	}
	// 更新内容标记
	if err := os.WriteFile(filepath.Join(src, "v2.marker"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	pkg2, err := r.Install(src)
	if err != nil {
		t.Fatalf("same-source reinstall must be an update: %v", err)
	}
	if _, err := os.Stat(filepath.Join(pkg2.Dir, "v2.marker")); err != nil {
		t.Fatal("update must replace package dir contents")
	}
	if !pkg2.Disabled() {
		t.Fatal("update must preserve disabled state")
	}
	if _, _, err := invoke(t, reg, "hello", nil, ""); err == nil {
		t.Fatal("disabled root command must fail explicitly after update")
	}
	if err := r.SetDisabled("hello", false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := invoke(t, reg, "hello", nil, ""); err != nil {
		t.Fatalf("invoke after update: %v", err)
	}
	_ = pkg
}

func TestDisabledFailsExplicitly(t *testing.T) {
	r, reg := newTestRegistry(t, true)
	if _, err := r.Install(buildHelloPkg(t)); err != nil {
		t.Fatal(err)
	}
	if err := r.SetDisabled("hello", true); err != nil {
		t.Fatal(err)
	}
	// 根命令保留注册（不回落系统程序）但显式失败
	if _, ok := reg.Lookup("hello"); !ok {
		t.Fatal("disabled package must keep root command registered")
	}
	_, _, err := invoke(t, reg, "hello", nil, "")
	if ee, ok := err.(*commands.ExitError); !ok || ee.Code != 126 {
		t.Fatalf("disabled invoke err = %v, want ExitError(126)", err)
	}
	if err := r.SetDisabled("hello", false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := invoke(t, reg, "hello", nil, ""); err != nil {
		t.Fatalf("re-enabled invoke: %v", err)
	}
}

func TestUninstall(t *testing.T) {
	r, reg := newTestRegistry(t, true)
	pkg, err := r.Install(buildHelloPkg(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Uninstall("hello"); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Lookup("hello"); ok {
		t.Fatal("uninstall must unregister root command")
	}
	if _, err := os.Stat(pkg.Dir); !os.IsNotExist(err) {
		t.Fatal("uninstall must remove package dir")
	}
	// 幂等
	if err := r.Uninstall("hello"); err != nil {
		t.Fatal(err)
	}
}

// TestSandboxFileBoundary 进程沙箱文件边界两路径：包目录（非白名单）写拒绝、
// 临时目录（白名单）写放行（aic/docs/skill.md §9.2：文件权限 = 进程沙箱）。
func TestSandboxFileBoundary(t *testing.T) {
	if !sandboxUsable() {
		t.Skip("sandbox backend unavailable（嵌套沙箱环境；真机直跑覆盖本测试）")
	}
	r, reg := newTestRegistry(t, false)
	pkg, err := r.Install(buildHelloPkg(t))
	if err != nil {
		t.Fatal(err)
	}
	// 拒绝路径：包安装目录（$HOME/.aic/skills 同口径——不在写白名单）
	stdout, _, err := invoke(t, reg, "hello", []string{"probe-write", filepath.Join(pkg.Dir, "tamper.txt")}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stdout, "denied:") {
		t.Fatalf("package dir write = %q, want denied", stdout)
	}
	// 放行路径：当次工作区（白名单）
	workdir := t.TempDir()
	target := filepath.Join(workdir, "probe-ok.txt")
	stdout, _, err = invokeAt(t, reg, workdir, "hello", []string{"probe-write", target}, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(stdout) != "ok" {
		t.Fatalf("workdir write = %q, want ok", stdout)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("probe file not written: %v", err)
	}
}

// zipDir 把目录打包为内存 zip（Download 测试的假 fetch 负载）。
func zipDir(t *testing.T, srcDir string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	err := filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		w, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
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
	return buf.Bytes()
}

// fakeFetch 构造假 Fetch dep：返回 hello 包 zip + 指定来源身份；mutate 可在
// 打包前改动包内容（更新场景）。
func fakeFetch(t *testing.T, id, version string, mutate func(pkgDir string)) func(context.Context, string, string) ([]byte, *FetchMeta, error) {
	t.Helper()
	return func(ctx context.Context, ref, ver string) ([]byte, *FetchMeta, error) {
		src := buildHelloPkg(t)
		if mutate != nil {
			mutate(src)
		}
		return zipDir(t, src), &FetchMeta{Name: "hello", Kind: "public", ID: id, Version: version}, nil
	}
}

// newTestRegistryFetch 同 newTestRegistry，注入 Fetch dep（Download 测试）。
func newTestRegistryFetch(t *testing.T, noSandbox bool, fetch func(context.Context, string, string) ([]byte, *FetchMeta, error)) (*Registry, *commands.Registry) {
	t.Helper()
	reg := commands.NewRegistry()
	m := vbox.NewManager(5 * time.Minute)
	m.SetNoSandbox(noSandbox)
	r, err := New(Deps{
		SkillsDir: filepath.Join(t.TempDir(), "skills"),
		RunDir:    shortRunDir(t),
		Manager:   m,
		Policy: func(ctx context.Context, workdir, name string) vshglue.NativePolicy {
			return vshglue.NativePolicy{WriteRoots: canonicalRoots(workdir)}
		},
		Workdir:  func(invCwd string) string { return invCwd },
		Registry: func() (*commands.Registry, error) { return reg, nil },
		Fetch:    fetch,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r, reg
}

// TestDownload 全链路：fetch zip → 原子安装 → .install.json 提交标记 → 注册可调用。
func TestDownload(t *testing.T) {
	r, reg := newTestRegistryFetch(t, true, fakeFetch(t, "uuid-1", "1.0.0", nil))
	pkg, err := r.Download(context.Background(), "uuid-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Name != "hello" {
		t.Fatalf("pkg.Name = %q, want hello", pkg.Name)
	}
	// .install.json 提交标记
	rec := readRecord(pkg.Dir)
	if rec == nil {
		t.Fatal("install record missing")
	}
	if rec.Kind != "public" || rec.ID != "uuid-1" || rec.Version != "1.0.0" {
		t.Fatalf("record = %+v", rec)
	}
	stdout, _, err := invoke(t, reg, "hello", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(stdout) != "hello from aic skill" {
		t.Fatalf("stdout = %q", stdout)
	}
}

// TestDownloadUpdateAndSourceConflict 同源更新 + 异源显式拒绝。
func TestDownloadUpdateAndSourceConflict(t *testing.T) {
	r, _ := newTestRegistryFetch(t, true, fakeFetch(t, "uuid-1", "1.0.0", nil))
	if _, err := r.Download(context.Background(), "uuid-1", ""); err != nil {
		t.Fatal(err)
	}
	if err := r.SetDisabled("hello", true); err != nil {
		t.Fatal(err)
	}
	// 同源更新（新版本 + 内容变化）
	r.deps.Fetch = fakeFetch(t, "uuid-1", "2.0.0", func(pkgDir string) {
		if err := os.WriteFile(filepath.Join(pkgDir, "v2.marker"), []byte("v2"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	pkg, err := r.Download(context.Background(), "uuid-1", "")
	if err != nil {
		t.Fatalf("same-source download must be an update: %v", err)
	}
	if _, err := os.Stat(filepath.Join(pkg.Dir, "v2.marker")); err != nil {
		t.Fatal("update must replace package dir contents")
	}
	if rec := readRecord(pkg.Dir); rec == nil || rec.Version != "2.0.0" || !rec.Disabled {
		t.Fatalf("record after update = %+v", rec)
	}
	// 异源同名 → 显式拒绝
	r.deps.Fetch = fakeFetch(t, "uuid-2", "1.0.0", nil)
	if _, err := r.Download(context.Background(), "uuid-2", ""); err == nil {
		t.Fatal("different-source download must be rejected")
	}
}

// TestDownloadBadZip 坏包显式失败且不残留注册。
func TestDownloadBadZip(t *testing.T) {
	r, reg := newTestRegistryFetch(t, true, func(ctx context.Context, ref, ver string) ([]byte, *FetchMeta, error) {
		return []byte("not a zip"), &FetchMeta{Name: "hello", Kind: "public", ID: "uuid-1"}, nil
	})
	if _, err := r.Download(context.Background(), "uuid-1", ""); err == nil {
		t.Fatal("bad zip must fail")
	}
	if _, ok := reg.Lookup("hello"); ok {
		t.Fatal("failed download must not register command")
	}
	// cli/ 存在但缺 manifest → 显式失败
	r.deps.Fetch = func(ctx context.Context, ref, ver string) ([]byte, *FetchMeta, error) {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		w, _ := zw.Create("cli/bin/x")
		_, _ = w.Write([]byte("x"))
		_ = zw.Close()
		return buf.Bytes(), &FetchMeta{Name: "hello", Kind: "public", ID: "uuid-1"}, nil
	}
	if _, err := r.Download(context.Background(), "uuid-1", ""); err == nil {
		t.Fatal("cli/ without manifest must fail")
	}
}

// TestDownloadArtifactFailure artifacts.lock 下载失败 = 安装失败（不写提交标记）。
func TestDownloadArtifactFailure(t *testing.T) {
	r, _ := newTestRegistryFetch(t, true, func(ctx context.Context, ref, ver string) ([]byte, *FetchMeta, error) {
		src := buildHelloPkg(t)
		// 不可达 https 地址（127.0.0.1:1 连接即拒）
		lock := `{"artifacts":[{"path":"bin/big.bin","url":"https://127.0.0.1:1/x","sha256":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}]}`
		if err := os.WriteFile(filepath.Join(src, "cli", "artifacts.lock.json"), []byte(lock), 0o644); err != nil {
			t.Fatal(err)
		}
		return zipDir(t, src), &FetchMeta{Name: "hello", Kind: "public", ID: "uuid-1"}, nil
	})
	if _, err := r.Download(context.Background(), "uuid-1", ""); err == nil {
		t.Fatal("artifact fetch failure must fail the install")
	}
	if rec := readRecord(filepath.Join(r.deps.SkillsDir, "hello")); rec != nil {
		t.Fatal("failed install must not leave commit marker")
	}
}

// TestRescan 启动扫描：有效 .install.json 重注册（禁用态恢复），半包跳过。
func TestRescan(t *testing.T) {
	skillsDir := filepath.Join(t.TempDir(), "skills")
	fetch := fakeFetch(t, "uuid-1", "1.0.0", nil)
	reg1 := commands.NewRegistry()
	m1 := vbox.NewManager(5 * time.Minute)
	m1.SetNoSandbox(true)
	r1, err := New(Deps{
		SkillsDir: skillsDir,
		RunDir:    shortRunDir(t),
		Manager:   m1,
		Workdir:   func(s string) string { return s },
		Registry:  func() (*commands.Registry, error) { return reg1, nil },
		Fetch:     fetch,
	})
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := r1.Download(context.Background(), "uuid-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := r1.SetDisabled("hello", true); err != nil {
		t.Fatal(err)
	}
	// 半包：无 .install.json 的目录
	half := filepath.Join(skillsDir, "half")
	if err := os.MkdirAll(filepath.Join(half, "cli"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(pkg.Dir, "cli", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(half, "cli", "manifest.json"), manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	// 新 Registry + 新命令表（= 重启）
	reg2 := commands.NewRegistry()
	m2 := vbox.NewManager(5 * time.Minute)
	m2.SetNoSandbox(true)
	r2, err := New(Deps{
		SkillsDir: skillsDir,
		RunDir:    shortRunDir(t),
		Manager:   m2,
		Policy: func(ctx context.Context, workdir, name string) vshglue.NativePolicy {
			return vshglue.NativePolicy{WriteRoots: canonicalRoots(workdir)}
		},
		Workdir:  func(s string) string { return s },
		Registry: func() (*commands.Registry, error) { return reg2, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	r2.Rescan()
	if _, ok := reg2.Lookup("hello"); !ok {
		t.Fatal("rescan must re-register installed package")
	}
	if !r2.Get("hello").Disabled() {
		t.Fatal("rescan must restore disabled state")
	}
	if r2.Get("half") != nil {
		t.Fatal("half package (no install record) must not register")
	}
	// 启用后可调用
	if err := r2.SetDisabled("hello", false); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := invoke(t, reg2, "hello", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(stdout) != "hello from aic skill" {
		t.Fatalf("stdout = %q", stdout)
	}
}

// TestRecordsJSON skill list --json 数据源：排序 + 字段完整。
func TestRecordsJSON(t *testing.T) {
	r, _ := newTestRegistryFetch(t, true, fakeFetch(t, "uuid-1", "1.0.0", nil))
	if _, err := r.Download(context.Background(), "uuid-1", ""); err != nil {
		t.Fatal(err)
	}
	js, err := r.RecordsJSON()
	if err != nil {
		t.Fatal(err)
	}
	var recs []InstallRecord
	if err := json.Unmarshal([]byte(js), &recs); err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Name != "hello" || recs[0].Kind != "public" || recs[0].ID != "uuid-1" || recs[0].Version != "1.0.0" {
		t.Fatalf("records = %s", js)
	}
}

func TestCancelKillsProcess(t *testing.T) {
	r, reg := newTestRegistry(t, true)
	if _, err := r.Install(buildHelloPkg(t)); err != nil {
		t.Fatal(err)
	}
	cmd, _ := reg.Lookup("hello")
	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr bytes.Buffer
	inv := commands.NewInvocation(&commands.InvocationOptions{
		Args:   []string{"sleep", "30"},
		Cwd:    t.TempDir(),
		Stdout: &stdout,
		Stderr: &stderr,
	})
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- cmd.Run(ctx, inv) }()
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled invoke must return error")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("cancel took %v, want prompt kill", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancel did not terminate provider process")
	}
}
