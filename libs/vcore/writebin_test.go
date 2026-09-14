package vcore

import (
	"bytes"
	"errors"
	"testing"

	"github.com/veypi/aic-pod/libs/proto"
)

// writebin 用例（2026-09-12，RTC 直连 writebin op 执行体）：字节精确落盘、
// 父目录自动创建、整文件覆写、空内容、权限门（deny 恒拒，granted=9 也不放行）。
// 环境构造与权限桩复用 readbin 用例（readBinEnv / denyAllPolicy）。

func TestWriteBinRoundTripWithMkdir(t *testing.T) {
	env, vfs := readBinEnv(t, nil)
	// 负载特征：PNG 魔数 + 全字节值循环（含 0x00/0xFF 等不可打印字节）
	payload := append([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A},
		bytes.Repeat([]byte{0xAB}, 1024)...)
	for i := range payload {
		payload[i] = byte(i)
	}
	payload[0] = 0x89                                // 保持 PNG 头不被循环覆盖（防误读为文本路径用例）
	n, err := WriteBin(env, "/a/b/out.png", payload) // 父目录不存在 → 自动创建
	if err != nil || n != len(payload) {
		t.Fatalf("write: n=%d err=%v", n, err)
	}
	got, err := vfs.ReadFile("/a/b/out.png")
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch: got %d bytes err=%v", len(got), err)
	}
}

func TestWriteBinOverwriteAndEmpty(t *testing.T) {
	env, vfs := readBinEnv(t, nil)
	if err := vfs.WriteFile("/f.bin", []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n, err := WriteBin(env, "/f.bin", []byte("new")); err != nil || n != 3 {
		t.Fatalf("overwrite: n=%d err=%v", n, err)
	}
	got, _ := vfs.ReadFile("/f.bin")
	if string(got) != "new" {
		t.Fatalf("overwrite mismatch: %q", got)
	}
	// 空内容合法（0 字节文件；nil 与空切片等价）
	if n, err := WriteBin(env, "/empty.bin", nil); err != nil || n != 0 {
		t.Fatalf("empty: n=%d err=%v", n, err)
	}
	st, err := vfs.Stat("/empty.bin")
	if err != nil || st.Size() != 0 {
		t.Fatalf("empty stat: %v err=%v", st, err)
	}
}

// 权限门：deny 恒拒（granted=9 也不放行——DeniedError 不可审批绕过）。
func TestWriteBinDenied(t *testing.T) {
	env, _ := readBinEnv(t, denyAllPolicy{})
	_, err := WriteBin(env, "/secret.bin", []byte("xx"))
	if err == nil {
		t.Fatal("denied path must error")
	}
	var denied *proto.DeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("want DeniedError, got %T: %v", err, err)
	}
}

func TestWriteBinEmptyPath(t *testing.T) {
	env, _ := readBinEnv(t, nil)
	if _, err := WriteBin(env, "", []byte("x")); err == nil {
		t.Fatal("empty path must error")
	}
}
