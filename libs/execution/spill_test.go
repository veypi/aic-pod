package execution

// spill_test.go 锁定惰性落盘语义（2026-10-07 裁定）：小输出不触盘；溢出
// 或强制才建文件；执行晚于响应返回时 Write 与 Spill/Close 并发安全；
// Close 后仍可 Spill 补建（同步完成的行截断收口）。

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestSpillerSmallOutputNeverTouchesDisk(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ".exec", "x.stdout.log")
	s := NewLogSpiller(path, 1024)
	if _, err := s.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if s.Spilled() {
		t.Fatal("small output must not spill")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file must not exist: %v", err)
	}
	if n, err := s.Size(); err != nil || n != 6 {
		t.Fatalf("buffered size = %d, %v", n, err)
	}
}

func TestSpillerOverflowStreamsFullOutput(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "x.log")
	s := NewLogSpiller(path, 16)
	payload := strings.Repeat("a", 100)
	if _, err := s.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	if !s.Spilled() {
		t.Fatal("overflow must spill")
	}
	// 溢出后继续写直落文件。
	if _, err := s.Write([]byte("tail")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != payload+"tail" {
		t.Fatalf("file = %d bytes, %v", len(data), err)
	}
	if n, _ := s.Size(); n != int64(len(payload)+4) {
		t.Fatalf("size = %d", n)
	}
}

func TestSpillerForceSpillWhileRunning(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "x.log")
	s := NewLogSpiller(path, 1<<20)
	if _, err := s.Write([]byte("prefix\n")); err != nil {
		t.Fatal(err)
	}
	// 转后台收口：响应发出前强制落盘，路径存在且持续可写。
	if err := s.Spill(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("forced spill must create file: %v", err)
	}
	if _, err := s.Write([]byte("suffix\n")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "prefix\nsuffix\n" {
		t.Fatalf("file = %q", data)
	}
	// 幂等。
	if err := s.Spill(); err != nil {
		t.Fatal(err)
	}
}

func TestSpillerPostCloseSpillWritesBufferedPrefix(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "x.log")
	s := NewLogSpiller(path, 1<<20)
	if _, err := s.Write([]byte("buffered full output\n")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// 同步完成后才判定行截断：Close 后 Spill 补建文件，内容完整。
	if err := s.Spill(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "buffered full output\n" {
		t.Fatalf("file = %q, %v", data, err)
	}
}

func TestSpillerConcurrentWriteAndSpill(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "x.log")
	s := NewLogSpiller(path, 4096)
	var wg sync.WaitGroup
	chunk := strings.Repeat("b", 500)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if _, err := s.Write([]byte(chunk)); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 20; j++ {
			_ = s.Spill() // 与 Write 竞争（转后台收口与执行续写并发）
		}
	}()
	wg.Wait()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 8*50*len(chunk) {
		t.Fatalf("file = %d bytes, want %d（写丢失或重复）", len(data), 8*50*len(chunk))
	}
}

func TestSpillerWriteFailureIsSticky(t *testing.T) {
	t.Parallel()
	// 父路径是不可建的文件 → spill 必败。
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewLogSpiller(filepath.Join(blocker, "x.log"), 4)
	if _, err := s.Write([]byte("overflow!")); err == nil {
		t.Fatal("spill failure must surface")
	}
	if s.Spilled() || s.Err() == nil {
		t.Fatal("failure must be sticky and visible")
	}
	if _, err := s.Write([]byte("again")); err == nil {
		t.Fatal("sticky error must fail subsequent writes")
	}
	if err := s.Spill(); err == nil {
		t.Fatal("sticky error must fail forced spill")
	}
}
