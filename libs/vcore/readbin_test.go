package vcore

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/veypi/aic-pod/libs/proto"
)

// readbin 用例（2026-09-10，RTC 直连 readbin op 执行体）：字节区间精确性、
// 边界与错误、权限门（deny 恒拒）、mime 探测。权限桩复用 fakePolicy。

// denyAllPolicy 是 PathPolicy 桩：全路径 deny（read/write 均 0 级）。
type denyAllPolicy struct{}

func (denyAllPolicy) Decide(string) (int, int) { return 0, 0 }

// readBinEnv 构造 ReadBin 测试环境：memfs + 可选策略。
func readBinEnv(t *testing.T, pol PathPolicy) (*Env, *MemVFS) {
	t.Helper()
	vfs := NewMemVFS()
	return &Env{VFS: vfs, Workdir: "/", Granted: 9, Policy: pol}, vfs
}

func TestReadBinRangeExact(t *testing.T) {
	env, vfs := readBinEnv(t, nil)
	content := []byte("0123456789abcdef") // 16B
	if err := vfs.WriteFile("/a.bin", content, 0o644); err != nil {
		t.Fatal(err)
	}
	// 全量
	data, mime, total, err := ReadBin(env, "/a.bin", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, content) || total != 16 {
		t.Fatalf("full read: len=%d total=%d", len(data), total)
	}
	if !strings.HasPrefix(mime, "text/") {
		t.Fatalf("mime: %s", mime)
	}
	// 中段区间
	data, _, total, err = ReadBin(env, "/a.bin", 4, 6)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "456789" || total != 16 {
		t.Fatalf("range read: %q total=%d", data, total)
	}
	// len 超尾 → 收拢到 EOF
	data, _, _, err = ReadBin(env, "/a.bin", 14, 100)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "ef" {
		t.Fatalf("tail clamp: %q", data)
	}
}

func TestReadBinBounds(t *testing.T) {
	env, vfs := readBinEnv(t, nil)
	if err := vfs.WriteFile("/a.bin", []byte("abcd"), 0o644); err != nil {
		t.Fatal(err)
	}
	// off == 文件尾：合法空读
	data, _, total, err := ReadBin(env, "/a.bin", 4, 0)
	if err != nil || len(data) != 0 || total != 4 {
		t.Fatalf("off==EOF: len=%d total=%d err=%v", len(data), total, err)
	}
	// off 越界 → 明确错误
	if _, _, _, err = ReadBin(env, "/a.bin", 5, 0); err == nil {
		t.Fatal("off > size must error")
	}
	// 负 off / 负 len → 明确错误
	if _, _, _, err = ReadBin(env, "/a.bin", -1, 0); err == nil {
		t.Fatal("negative off must error")
	}
	if _, _, _, err = ReadBin(env, "/a.bin", 0, -1); err == nil {
		t.Fatal("negative len must error")
	}
	// 目录 → 明确错误
	if err = vfs.MkdirAll("/d", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = ReadBin(env, "/d", 0, 0); err == nil {
		t.Fatal("directory must error")
	}
	// 空 path → 明确错误
	if _, _, _, err = ReadBin(env, "", 0, 0); err == nil {
		t.Fatal("empty path must error")
	}
}

// 权限门：deny 恒拒（granted=9 也不放行——DeniedError 不可审批绕过）。
func TestReadBinDenied(t *testing.T) {
	env, vfs := readBinEnv(t, denyAllPolicy{})
	if err := vfs.WriteFile("/secret.bin", []byte("xx"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := ReadBin(env, "/secret.bin", 0, 0)
	if err == nil {
		t.Fatal("denied path must error")
	}
	var denied *proto.DeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("want DeniedError, got %T: %v", err, err)
	}
}

// mime 按文件头探测（PNG magic），off>0 时补读头部不用区间数据猜。
func TestReadBinMIME(t *testing.T) {
	env, vfs := readBinEnv(t, nil)
	png := append([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}, bytes.Repeat([]byte{1}, 100)...)
	if err := vfs.WriteFile("/i.png", png, 0o644); err != nil {
		t.Fatal(err)
	}
	_, mime, _, err := ReadBin(env, "/i.png", 0, 0)
	if err != nil || mime != "image/png" {
		t.Fatalf("head detect: mime=%q err=%v", mime, err)
	}
	_, mime, _, err = ReadBin(env, "/i.png", 50, 10)
	if err != nil || mime != "image/png" {
		t.Fatalf("off>0 detect: mime=%q err=%v", mime, err)
	}
}
