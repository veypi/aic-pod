package vsh

import (
	"context"
	"errors"
	"io"
	stdfs "io/fs"
	"os"
	"strings"
	"testing"

	"github.com/veypi/vbox"
	gbfs "github.com/veypi/vsh/fs"
	"github.com/veypi/vigo/contrib/ufs"
)

// newCloudAdapter 构造 cloud 适配器：localFS backing + jail /u/u1 +
// 规则表（便利根 rw 会话目录 → ro 用户根；DefaultWrite deny）。
func newCloudAdapter(t *testing.T) (gbfs.FileSystem, ufs.FS) {
	t.Helper()
	backing, err := ufs.NewLocalFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rules := vbox.NewFSRuleSet([]vbox.Rule{
		{Pattern: "/u/u1/.sessions/s1", Effect: vbox.EffRW, Class: vbox.ClassConvenience},
		{Pattern: "/u/u1", Effect: vbox.EffRO, Class: vbox.ClassCfg},
	}, vbox.EffDeny)
	fsys, err := NewCloudFS(CloudFSConfig{
		UserRoot: "/u/u1",
		Backing:  backing,
		Rules:    func() vbox.FSRuleSet { return rules },
	})
	if err != nil {
		t.Fatal(err)
	}
	return fsys, backing
}

func writeFile(t *testing.T, fsys gbfs.FileSystem, name, data string) error {
	t.Helper()
	f, err := fsys.OpenFile(context.Background(), name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write([]byte(data))
	return err
}

func TestCloudFSJailHardConstraint(t *testing.T) {
	t.Parallel()
	fsys, _ := newCloudAdapter(t)
	ctx := context.Background()
	// jail 外读/写一律拒（规则表管不到界外——代码硬约束）。
	// /etc、/tmp 等是内存层（per-session scratch），不在此用例范围。
	if _, err := fsys.Open(ctx, "/u/other/secrets.txt"); !errors.Is(err, ErrOutsideJail) {
		t.Fatalf("cross-user read = %v, want ErrOutsideJail", err)
	}
	f, err := fsys.OpenFile(ctx, "/u/other/x.txt", os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		f.Close()
		t.Fatalf("cross-user write should fail")
	}
}

func TestCloudFSRuleTableGate(t *testing.T) {
	t.Parallel()
	fsys, backing := newCloudAdapter(t)
	ctx := context.Background()
	// 便利根 rw：会话目录可写（首命中生效压过 ro 行——行序回归）。
	if err := writeFile(t, fsys, "/u/u1/.sessions/s1/out.txt", "ok"); err != nil {
		t.Fatalf("session write = %v", err)
	}
	data, err := backing.ReadFile("/u/u1/.sessions/s1/out.txt")
	if err != nil || string(data) != "ok" {
		t.Fatalf("backing read = %q %v", data, err)
	}
	// ro 行：用户根其他位置写拒绝、读开放。
	if err := writeFile(t, fsys, "/u/u1/docs/a.md", "x"); !errors.Is(err, ErrRuleDenied) {
		t.Fatalf("ro write = %v, want ErrRuleDenied", err)
	}
	if err := backing.MkdirAll("/u/u1/docs", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := backing.WriteFile("/u/u1/docs/a.md", []byte("ro-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.Open(ctx, "/u/u1/docs/a.md"); err != nil {
		t.Fatalf("ro read should pass: %v", err)
	}
	// ro 行同样覆盖未单列子路径（写拒、报错引导 grant）。
	err = writeFile(t, fsys, "/u/u1/unlisted/x", "x")
	if !errors.Is(err, ErrRuleDenied) {
		t.Fatalf("unlisted write = %v, want ErrRuleDenied", err)
	}
	if !strings.Contains(err.Error(), "grant fs") {
		t.Fatalf("error should guide grant: %v", err)
	}
}

func TestCloudFSMemoryLayerNoPollution(t *testing.T) {
	t.Parallel()
	fsys, backing := newCloudAdapter(t)
	ctx := context.Background()
	// 内存层写：/tmp、/bin 自由读写，且不落 UFS（红线）。
	if err := writeFile(t, fsys, "/tmp/scratch.txt", "tmp"); err != nil {
		t.Fatalf("/tmp write = %v", err)
	}
	if err := writeFile(t, fsys, "/bin/mystub", "#!stub"); err != nil {
		t.Fatalf("/bin stub write = %v", err)
	}
	f, err := fsys.Open(ctx, "/tmp/scratch.txt")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(f)
	f.Close()
	if string(data) != "tmp" {
		t.Fatalf("mem read = %q", data)
	}
	// UFS 直通侧不可见（红线：无 stub 污染）。
	if _, err := backing.Stat("/tmp/scratch.txt"); !errors.Is(err, stdfs.ErrNotExist) {
		t.Fatalf("UFS polluted by /tmp: %v", err)
	}
	if _, err := backing.Stat("/bin/mystub"); !errors.Is(err, stdfs.ErrNotExist) {
		t.Fatalf("UFS polluted by /bin: %v", err)
	}
}

func TestCloudFSStubsSeeded(t *testing.T) {
	t.Parallel()
	backing, err := ufs.NewLocalFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fsys, err := NewCloudFS(CloudFSConfig{
		UserRoot: "/u/u1",
		Backing:  backing,
		Stubs:    map[string][]byte{"/usr/bin/python": []byte("#! stub: python 未授权\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	f, err := fsys.Open(context.Background(), "/usr/bin/python")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(f)
	f.Close()
	if !strings.Contains(string(data), "stub") {
		t.Fatalf("stub = %q", data)
	}
}

func TestCloudFSRemoveRmdirSemantics(t *testing.T) {
	t.Parallel()
	fsys, backing := newCloudAdapter(t)
	ctx := context.Background()
	if err := writeFile(t, fsys, "/u/u1/.sessions/s1/dir/f.txt", "x"); err != nil {
		t.Fatal(err)
	}
	// 非空目录非递归删 → 拒（rmdir 语义补齐）。
	if err := fsys.Remove(ctx, "/u/u1/.sessions/s1/dir", false); err == nil {
		t.Fatal("non-recursive remove of non-empty dir should fail")
	}
	if err := fsys.Remove(ctx, "/u/u1/.sessions/s1/dir", true); err != nil {
		t.Fatal(err)
	}
	if _, err := backing.Stat("/u/u1/.sessions/s1/dir"); !errors.Is(err, stdfs.ErrNotExist) {
		t.Fatalf("dir should be gone: %v", err)
	}
}

func TestCloudFSRenameGated(t *testing.T) {
	t.Parallel()
	fsys, _ := newCloudAdapter(t)
	ctx := context.Background()
	if err := writeFile(t, fsys, "/u/u1/.sessions/s1/a.txt", "x"); err != nil {
		t.Fatal(err)
	}
	// 目标出写域 → 拒。
	if err := fsys.Rename(ctx, "/u/u1/.sessions/s1/a.txt", "/u/u1/docs/b.txt"); !errors.Is(err, ErrRuleDenied) {
		t.Fatalf("rename out of write domain = %v", err)
	}
	if err := fsys.Rename(ctx, "/u/u1/.sessions/s1/a.txt", "/u/u1/.sessions/s1/b.txt"); err != nil {
		t.Fatal(err)
	}
}

func TestCloudFSAppend(t *testing.T) {
	t.Parallel()
	fsys, _ := newCloudAdapter(t)
	ctx := context.Background()
	if err := writeFile(t, fsys, "/u/u1/.sessions/s1/log.txt", "a"); err != nil {
		t.Fatal(err)
	}
	f, err := fsys.OpenFile(ctx, "/u/u1/.sessions/s1/log.txt", os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("b")); err != nil {
		t.Fatal(err)
	}
	f.Close()
	rf, err := fsys.Open(ctx, "/u/u1/.sessions/s1/log.txt")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(rf)
	rf.Close()
	if string(data) != "ab" {
		t.Fatalf("append = %q", data)
	}
}

func TestCloudFSChdirGetwd(t *testing.T) {
	t.Parallel()
	fsys, _ := newCloudAdapter(t)
	if err := writeFile(t, fsys, "/u/u1/.sessions/s1/x.txt", "x"); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Chdir(".sessions/s1"); err != nil {
		t.Fatal(err)
	}
	if wd := fsys.Getwd(); wd != "/u/u1/.sessions/s1" {
		t.Fatalf("wd = %q", wd)
	}
	// 相对路径写落 cwd。
	if err := writeFile(t, fsys, "y.txt", "y"); err != nil {
		t.Fatal(err)
	}
	f, err := fsys.Open(context.Background(), "/u/u1/.sessions/s1/y.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
}
