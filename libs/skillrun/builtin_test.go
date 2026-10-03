package skillrun

// builtin 首跑预装测试（v6 P5）：首装 / 幂等跳过 / 同名异源报错语义与
// Download 一致 / 坏 zip 不致命。

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	aicskills "github.com/veypi/aic-skills"
)

func TestEmbeddedCatalogInstallsWithoutErrors(t *testing.T) {
	r, _ := newTestRegistry(t, true)
	var logs strings.Builder
	r.deps.Logf = func(format string, args ...any) { fmt.Fprintf(&logs, format+"\n", args...) }
	r.PreinstallEmbedded(context.Background())
	for _, name := range aicskills.List() {
		if pkg := r.Get(name); pkg == nil || pkg.Record().Version != aicskills.Version(name) {
			t.Fatalf("complete builtin %s not installed: %s", name, logs.String())
		}
	}
	if strings.Contains(logs.String(), "embedded builtin") {
		t.Fatalf("builtin installation failed: %s", logs.String())
	}
}

// builtinZip 装配 hello 包（含 SKILL.md frontmatter name/version）并打成 zip 文件。
func builtinZip(t *testing.T, name, version string) []byte {
	t.Helper()
	src := buildHelloPkg(t)
	doc := "---\nname: " + name + "\nversion: " + version + "\ndescription: test builtin\nui:\n  - path: index.html\n---\n\n# " + name + "\n"
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return zipDir(t, src)
}

// TestPreinstallBuiltin 首装：zip → installZip → 记录 kind=builtin id=包名，
// 根命令注册可调用。
func TestPreinstallBuiltin(t *testing.T) {
	r, reg := newTestRegistry(t, true)
	if err := r.installBuiltinZip(context.Background(), builtinZip(t, "hello", "1.0.0"), "test"); err != nil {
		t.Fatal(err)
	}
	pkg := r.Get("hello")
	if pkg == nil {
		t.Fatal("builtin package not installed")
	}
	rec := pkg.Record()
	if rec.Kind != KindBuiltin || rec.ID != "hello" || rec.Version != "1.0.0" {
		t.Fatalf("record = %+v", rec)
	}
	stdout, _, err := invoke(t, reg, "hello", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if stdout == "" {
		t.Fatal("installed builtin command must run")
	}
}

// TestPreinstallIdempotent 同源同名同版本 → 跳过（包目录不被替换）。
func TestPreinstallIdempotent(t *testing.T) {
	r, _ := newTestRegistry(t, true)
	zipPath := builtinZip(t, "hello", "1.0.0")
	if err := r.installBuiltinZip(context.Background(), zipPath, "test"); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(r.Get("hello").Dir, "keep.marker")
	if err := os.WriteFile(marker, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.installBuiltinZip(context.Background(), zipPath, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("same-version builtin preinstall must skip (package dir replaced)")
	}
	// 版本变化 → 同源更新（目录替换）
	if err := r.installBuiltinZip(context.Background(), builtinZip(t, "hello", "2.0.0"), "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("version bump must reinstall (marker should be gone)")
	}
	if rec := r.Get("hello").Record(); rec.Version != "2.0.0" {
		t.Fatalf("record after update = %+v", rec)
	}
}

// TestPreinstallSourceConflict 同名异源：报错语义与 Download 一致（显式拒绝、
// 不覆盖原安装），且只记日志不中断其余项。
func TestPreinstallSourceConflict(t *testing.T) {
	r, _ := newTestRegistryFetch(t, true, fakeFetch(t, "uuid-1", "1.0.0", nil))
	if _, err := r.Download(context.Background(), "uuid-1", ""); err != nil {
		t.Fatal(err)
	}
	if err := r.installBuiltinZip(context.Background(), builtinZip(t, "hello", "1.0.0"), "test"); err == nil {
		t.Fatal("source conflict must fail")
	}
	rec := r.Get("hello").Record()
	if rec.Kind != "public" || rec.ID != "uuid-1" {
		t.Fatalf("different-source builtin must not overwrite: %+v", rec)
	}
}

// TestPreinstallBadZipNotFatal 坏 zip / 缺 frontmatter 只记日志，后续好包照装。
func TestPreinstallRejectsInvalidPackage(t *testing.T) {
	r, _ := newTestRegistry(t, true)
	for _, data := range [][]byte{[]byte("not a zip"), zipDir(t, buildHelloPkg(t))} {
		if err := r.installBuiltinZip(context.Background(), data, "test"); err == nil {
			t.Fatal("invalid builtin accepted")
		}
	}
	if err := r.installBuiltinZip(context.Background(), builtinZip(t, "hello", "1.0.0"), "test"); err != nil {
		t.Fatal(err)
	}
}

// TestSkillFrontmatter frontmatter 解析：只取顶层 name/version 标量——
// 解析器唯一实现 = aicskills.Frontmatter，本测试经它验证契约。
func TestSkillFrontmatter(t *testing.T) {
	name, version := aicskills.Frontmatter([]byte("---\nname: browser\ndescription: x\nui:\n  - path: index.html\nversion: 1.2.3\n---\nbody"))
	if name != "browser" || version != "1.2.3" {
		t.Fatalf("frontmatter = %q %q", name, version)
	}
	if n, v := aicskills.Frontmatter([]byte("no frontmatter")); n != "" || v != "" {
		t.Fatalf("no frontmatter = %q %q", n, v)
	}
	// 缩进键（嵌套）不算顶层
	if n, _ := aicskills.Frontmatter([]byte("---\nui:\n  name: nested\n---\n")); n != "" {
		t.Fatalf("nested key must not leak: %q", n)
	}
}

func TestFailedBuiltinUpdatePreservesInstalledPackage(t *testing.T) {
	r, reg := newTestRegistry(t, true)
	if err := r.installBuiltinZip(context.Background(), builtinZip(t, "hello", "1.0.0"), "test"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := invoke(t, reg, "hello", nil, ""); err != nil {
		t.Fatal(err)
	}
	old := r.Get("hello")
	service := packageService(r, "hello")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{
		"SKILL.md":          "---\nname: hello\nversion: 2.0.0\n---\n",
		"cli/manifest.json": `{"kind":"service","entry":"cli/bin/missing"}`,
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	zw.Close()
	if err := r.installBuiltinZip(context.Background(), buf.Bytes(), "test"); err == nil {
		t.Fatal("missing provider must fail")
	}
	if r.Get("hello") != old || packageService(r, "hello") != service {
		t.Fatal("failed update changed installed package/service")
	}
	if rec := readRecord(old.Dir); rec == nil || rec.Version != "1.0.0" {
		t.Fatalf("lost installed record: %+v", rec)
	}
	if _, _, err := invoke(t, reg, "hello", nil, ""); err != nil {
		t.Fatalf("old command lost after failed update: %v", err)
	}
}
