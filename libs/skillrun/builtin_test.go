package skillrun

// builtin 首跑预装测试（v6 P5）：首装 / 幂等跳过 / 同名异源报错语义与
// Download 一致 / 坏 zip 不致命。

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// builtinZip 装配 hello 包（含 SKILL.md frontmatter name/version）并打成 zip 文件。
func builtinZip(t *testing.T, name, version string) string {
	t.Helper()
	src := buildHelloPkg(t)
	doc := "---\nname: " + name + "\nversion: " + version + "\ndescription: test builtin\nui:\n  - path: index.html\n---\n\n# " + name + "\n"
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	zipPath := filepath.Join(t.TempDir(), name+".zip")
	if err := os.WriteFile(zipPath, zipDir(t, src), 0o644); err != nil {
		t.Fatal(err)
	}
	return zipPath
}

// TestPreinstallBuiltin 首装：zip → installZip → 记录 kind=builtin id=包名，
// 根命令注册可调用。
func TestPreinstallBuiltin(t *testing.T) {
	r, reg := newTestRegistry(t, true)
	r.Preinstall(context.Background(), []string{builtinZip(t, "hello", "1.0.0")})
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
	r.Preinstall(context.Background(), []string{zipPath})
	marker := filepath.Join(r.Get("hello").Dir, "keep.marker")
	if err := os.WriteFile(marker, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.Preinstall(context.Background(), []string{zipPath})
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("same-version builtin preinstall must skip (package dir replaced)")
	}
	// 版本变化 → 同源更新（目录替换）
	r.Preinstall(context.Background(), []string{builtinZip(t, "hello", "2.0.0")})
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
	r.Preinstall(context.Background(), []string{builtinZip(t, "hello", "1.0.0")})
	rec := r.Get("hello").Record()
	if rec.Kind != "public" || rec.ID != "uuid-1" {
		t.Fatalf("different-source builtin must not overwrite: %+v", rec)
	}
}

// TestPreinstallBadZipNotFatal 坏 zip / 缺 frontmatter 只记日志，后续好包照装。
func TestPreinstallBadZipNotFatal(t *testing.T) {
	r, _ := newTestRegistry(t, true)
	bad := filepath.Join(t.TempDir(), "bad.zip")
	if err := os.WriteFile(bad, []byte("not a zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	noName := filepath.Join(t.TempDir(), "noname.zip")
	src := buildHelloPkg(t)
	if err := os.WriteFile(noName, zipDir(t, src), 0o644); err != nil { // 无 SKILL.md
		t.Fatal(err)
	}
	r.Preinstall(context.Background(), []string{bad, noName, builtinZip(t, "hello", "1.0.0")})
	if r.Get("hello") == nil {
		t.Fatal("bad entries must not block subsequent installs")
	}
}

// TestSkillFrontmatter frontmatter 解析：只取顶层 name/version 标量。
func TestSkillFrontmatter(t *testing.T) {
	name, version := skillFrontmatter([]byte("---\nname: browser\ndescription: x\nui:\n  - path: index.html\nversion: 1.2.3\n---\nbody"))
	if name != "browser" || version != "1.2.3" {
		t.Fatalf("frontmatter = %q %q", name, version)
	}
	if n, v := skillFrontmatter([]byte("no frontmatter")); n != "" || v != "" {
		t.Fatalf("no frontmatter = %q %q", n, v)
	}
	// 缩进键（嵌套）不算顶层
	if n, _ := skillFrontmatter([]byte("---\nui:\n  name: nested\n---\n")); n != "" {
		t.Fatalf("nested key must not leak: %q", n)
	}
}
