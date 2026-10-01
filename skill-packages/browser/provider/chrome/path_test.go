package chrome

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestProfileLockAndExplicitExecutable(t *testing.T) {
	dir := t.TempDir()
	unlock, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := Lock(dir); err == nil {
		second()
		t.Fatal("profile concurrently locked")
	}
	unlock()
	unlock, err = Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	exe := filepath.Join(dir, "chrome.exe")
	if err = os.WriteFile(exe, []byte("test"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIC_BROWSER_BUNDLE_DIR", "")
	t.Setenv("AIC_BROWSER_PATH", filepath.Join(dir, "missing"))
	if _, err = Resolve(""); err == nil {
		t.Fatal("invalid explicit path fell back silently")
	}
	if p, err := Resolve(exe); err != nil || p != exe {
		t.Fatalf("configured path precedence: %s %v", p, err)
	}
}

// TestBundleDirHit 打包器提示目录：{platform}-{arch}/ 布局命中（linux-x64 形态，
// 主机无关）。
func TestBundleDirHit(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "linux-x64", "chrome")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("test"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := func(key string) string {
		if key == "AIC_BROWSER_BUNDLE_DIR" {
			return dir
		}
		return ""
	}
	p, err := resolve("", "linux", "amd64", env)
	if err != nil || p != exe {
		t.Fatalf("bundle dir hit: %s %v", p, err)
	}
	// bundle 目录存在但布局缺失 → 不命中（fallback/报错由链尾决定）
	envMissing := func(key string) string {
		if key == "AIC_BROWSER_BUNDLE_DIR" {
			return filepath.Join(dir, "missing")
		}
		return ""
	}
	if _, err := resolve("", "windows", "amd64", envMissing); err == nil {
		t.Fatal("incomplete bundle dir must not resolve")
	}
}

// TestBundleExecutableMapping darwin .app / win32 chrome.exe / linux chrome 映射
// 与 desktop sync-browser 布局同口径。
func TestBundleExecutableMapping(t *testing.T) {
	got := bundleExecutable("/b", "darwin", "arm64")
	want := filepath.Join("/b", "darwin-arm64", "Google Chrome for Testing.app", "Contents", "MacOS", "Google Chrome for Testing")
	if got != want {
		t.Fatalf("darwin mapping: %s", got)
	}
	if got = bundleExecutable("/b", "windows", "amd64"); got != filepath.Join("/b", "win32-x64", "chrome.exe") {
		t.Fatalf("windows mapping: %s", got)
	}
	if got = bundleExecutable("/b", "linux", "amd64"); got != filepath.Join("/b", "linux-x64", "chrome") {
		t.Fatalf("linux mapping: %s", got)
	}
}

// TestSystemCandidateFallback 无显式/无 bundle → 系统候选（linux PATH 名）。
func TestSystemCandidateFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH 名可执行位语义为 unix")
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "google-chrome")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("AIC_BROWSER_PATH", "")
	t.Setenv("AIC_BROWSER_BUNDLE_DIR", "")
	p, err := resolve("", "linux", "amd64", os.Getenv)
	if err != nil || p != exe {
		t.Fatalf("system candidate fallback: %s %v", p, err)
	}
	// windows 系统候选走 PROGRAMFILES 系环境根
	root := t.TempDir()
	winExe := filepath.Join(root, "Google", "Chrome", "Application", "chrome.exe")
	if err := os.MkdirAll(filepath.Dir(winExe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(winExe, []byte("test"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := func(key string) string {
		if key == "PROGRAMFILES" {
			return root
		}
		return ""
	}
	if p, err = resolve("", "windows", "amd64", env); err != nil || p != winExe {
		t.Fatalf("windows system candidate: %s %v", p, err)
	}
}

// TestResolveNotFound 全部缺失 → 显式报错提示 AIC_BROWSER_PATH。
func TestResolveNotFound(t *testing.T) {
	t.Setenv("AIC_BROWSER_PATH", "")
	t.Setenv("AIC_BROWSER_BUNDLE_DIR", "")
	t.Setenv("PATH", t.TempDir())
	_, err := resolve("", "linux", "amd64", os.Getenv)
	if err == nil || !strings.Contains(err.Error(), "AIC_BROWSER_PATH") {
		t.Fatalf("not-found error must hint AIC_BROWSER_PATH: %v", err)
	}
}
