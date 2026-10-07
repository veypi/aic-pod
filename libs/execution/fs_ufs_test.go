package execution

import (
	"context"
	"errors"
	"io"
	stdfs "io/fs"
	"os"
	"strings"
	"testing"

	"github.com/veypi/vbox"
	"github.com/veypi/vigo/contrib/ufs"
	gbfs "github.com/veypi/vsh/fs"
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
		Aliases:  map[string]string{"/tmp": "/u/u1/.sessions/s1/tmp"},
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
	// /etc 不在 jail 内（与 host 语义对齐：写 /etc = 明确报错而非假成功）；
	// /tmp 经别名重定向进会话空间。
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
	// 内存层写：/proc 自由读写，且不落 UFS（红线）。
	if err := writeFile(t, fsys, "/proc/scratch.txt", "tmp"); err != nil {
		t.Fatalf("/proc write = %v", err)
	}
	f, err := fsys.Open(ctx, "/proc/scratch.txt")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(f)
	f.Close()
	if string(data) != "tmp" {
		t.Fatalf("mem read = %q", data)
	}
	// UFS 直通侧不可见（红线：伪系统目录不落持久层）。
	if _, err := backing.Stat("/proc/scratch.txt"); !errors.Is(err, stdfs.ErrNotExist) {
		t.Fatalf("UFS polluted by /proc: %v", err)
	}
}

// /tmp 别名重定向：写落会话空间（跨 exec 持久、过规则表门），与真实目录同语义。
func TestCloudFSTmpAliasToSessionSpace(t *testing.T) {
	t.Parallel()
	fsys, backing := newCloudAdapter(t)
	ctx := context.Background()
	if err := writeFile(t, fsys, "/tmp/scratch.txt", "tmp"); err != nil {
		t.Fatalf("/tmp write = %v", err)
	}
	// 落点在 backing 的会话 tmp 目录。
	data, err := backing.ReadFile("/u/u1/.sessions/s1/tmp/scratch.txt")
	if err != nil || string(data) != "tmp" {
		t.Fatalf("backing tmp read = %q %v", data, err)
	}
	// 读回一致。
	f, err := fsys.Open(ctx, "/tmp/scratch.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
}

// /etc 无内存层：写 = 明确报错（jail 外硬拒），不假成功。
func TestCloudFSEtcWriteDenied(t *testing.T) {
	t.Parallel()
	fsys, backing := newCloudAdapter(t)
	ctx := context.Background()
	if err := writeFile(t, fsys, "/etc/deny-test", "x"); !errors.Is(err, ErrOutsideJail) {
		t.Fatalf("/etc write = %v, want ErrOutsideJail", err)
	}
	if _, err := backing.Stat("/etc/deny-test"); !errors.Is(err, stdfs.ErrNotExist) {
		t.Fatalf("/etc must not land anywhere: %v", err)
	}
	_ = ctx
}

// 合成系统身份文件（SysFiles）：读与元数据由平台内容作答，写照旧硬拒、
// 不落 backing——/etc 其余路径语义不变（回归 2026-10-08：ls -l 的身份解析
// 每次产生越界拒绝回显）。
func TestCloudFSSysFilesReadOnly(t *testing.T) {
	t.Parallel()
	backing, err := ufs.NewLocalFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rules := vbox.NewFSRuleSet(nil, vbox.EffDeny)
	const content = "u1:x:1000:1000:u1:/u/u1:/bin/sh\n"
	fsys, err := NewCloudFS(CloudFSConfig{
		UserRoot: "/u/u1",
		Backing:  backing,
		Rules:    func() vbox.FSRuleSet { return rules },
		SysFiles: map[string][]byte{"/etc/passwd": []byte(content)},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// 读放行且内容为平台合成值。
	f, err := fsys.Open(ctx, "/etc/passwd")
	if err != nil {
		t.Fatalf("sysfile read = %v, want open", err)
	}
	data, err := io.ReadAll(f)
	f.Close()
	if err != nil || string(data) != content {
		t.Fatalf("sysfile content = %q %v, want %q", data, err, content)
	}
	// 元数据（stat/lstat/realpath）放行——否则命令的词法解析先挂。
	fi, err := fsys.Stat(ctx, "/etc/passwd")
	if err != nil || fi.IsDir() || fi.Size() != int64(len(content)) {
		t.Fatalf("sysfile stat = %+v %v", fi, err)
	}
	if _, err := fsys.Lstat(ctx, "/etc/passwd"); err != nil {
		t.Fatalf("sysfile lstat = %v", err)
	}
	if abs, err := fsys.Realpath(ctx, "/etc/passwd"); err != nil || abs != "/etc/passwd" {
		t.Fatalf("sysfile realpath = %q %v", abs, err)
	}
	// 句柄只读：内容写了也不生效。
	f, err = fsys.Open(ctx, "/etc/passwd")
	if err != nil {
		t.Fatal(err)
	}
	if _, werr := f.Write([]byte("x")); !errors.Is(werr, stdfs.ErrPermission) {
		t.Fatalf("sysfile write = %v, want EPERM", werr)
	}
	f.Close()
	// 写路径照旧硬拒（写意图的 OpenFile 不路由到合成层），且不落 backing。
	if err := writeFile(t, fsys, "/etc/passwd", "x"); !errors.Is(err, ErrOutsideJail) {
		t.Fatalf("/etc/passwd write = %v, want ErrOutsideJail", err)
	}
	if _, err := backing.Stat("/etc/passwd"); !errors.Is(err, stdfs.ErrNotExist) {
		t.Fatalf("sysfile must not land anywhere: %v", err)
	}
	// 未列入 SysFiles 的 /etc 路径语义不变（照旧越界拒）。
	if _, err := fsys.Open(ctx, "/etc/shadow"); !errors.Is(err, ErrOutsideJail) {
		t.Fatalf("/etc/shadow read = %v, want ErrOutsideJail", err)
	}
}

// 动态行（Resolve）的拒绝原因直达报错文案（2026-10-08）：调用方不再对公开包
// 给出「请 grant」的错误指引——真实情形（发布物不可写且不可授）只有回调知道。
func TestCloudFSDynamicRuleReason(t *testing.T) {
	t.Parallel()
	backing, err := ufs.NewLocalFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rules := vbox.NewFSRuleSet([]vbox.Rule{
		{Pattern: "/skills", Effect: vbox.EffDeny, Class: vbox.ClassCfg,
			Resolve: func(string, vbox.FileOp) vbox.Resolution {
				return vbox.Resolution{Handled: true, Effect: vbox.EffDeny, Reason: "公开包是发布物：任何人不允许改写，grant 也不可授"}
			}},
	}, vbox.EffDeny)
	fsys, err := NewCloudFS(CloudFSConfig{
		UserRoot:  "/u/u1",
		JailExtra: []string{"/skills"},
		Backing:   backing,
		Rules:     func() vbox.FSRuleSet { return rules },
	})
	if err != nil {
		t.Fatal(err)
	}
	err = writeFile(t, fsys, "/skills/pub1/SKILL.md", "x")
	if !errors.Is(err, ErrRuleDenied) {
		t.Fatalf("err = %v, want ErrRuleDenied", err)
	}
	if !strings.Contains(err.Error(), "公开包是发布物") {
		t.Fatalf("报错未带上动态行原因: %v", err)
	}
	// 静态行/兜底的统一文案仍在（报错引导 grant）。
	static := vbox.NewFSRuleSet(nil, vbox.EffDeny)
	fsys2, err := NewCloudFS(CloudFSConfig{
		UserRoot: "/u/u1", Backing: backing,
		Rules: func() vbox.FSRuleSet { return static },
	})
	if err != nil {
		t.Fatal(err)
	}
	err = writeFile(t, fsys2, "/u/u1/denied.txt", "x")
	if !errors.Is(err, ErrRuleDenied) || !strings.Contains(err.Error(), "grant fs") {
		t.Fatalf("静态拒绝文案 = %v, want 含 grant fs", err)
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

// F4 回归（2026-09-24 实测）：内存层 symlink 指向 backing 必须先 follow 后
// （链接落点用 /proc——/tmp 已改为会话空间别名，不再是内存层）。
// 路由——读拿到真实内容、写落 backing（不过影子）、ro 区经链接写被门拒。
func TestCloudFSSymlinkEscapeFollowsToBacking(t *testing.T) {
	t.Parallel()
	fsys, backing := newCloudAdapter(t)
	ctx := context.Background()

	// rw 区真实目录 + 文件（会话目录 rw）。
	if err := writeFile(t, fsys, "/u/u1/.sessions/s1/data/real.txt", "real"); err != nil {
		t.Fatal(err)
	}
	// 内存层链接 -> backing rw 区。
	if err := fsys.Symlink(ctx, "/u/u1/.sessions/s1/data", "/proc/link"); err != nil {
		t.Fatal(err)
	}

	// 读经链接 = 读真实内容。
	f, err := fsys.Open(ctx, "/proc/link/real.txt")
	if err != nil {
		t.Fatalf("read through link: %v", err)
	}
	data, _ := io.ReadAll(f)
	f.Close()
	if string(data) != "real" {
		t.Fatalf("read through link = %q", data)
	}

	// ReadDir 经链接 = backing 真实条目。
	entries, err := fsys.ReadDir(ctx, "/proc/link")
	if err != nil || len(entries) != 1 || entries[0].Name() != "real.txt" {
		t.Fatalf("readdir through link = %v, %v", entries, err)
	}

	// 写经链接落 backing（不落内存层影子）。
	if err := writeFile(t, fsys, "/proc/link/new.txt", "new"); err != nil {
		t.Fatalf("write through link: %v", err)
	}
	if fi, err := backing.Stat("/u/u1/.sessions/s1/data/new.txt"); err != nil || fi.IsDir() {
		t.Fatalf("write should land in backing: %v", err)
	}
	// 直接路径读回一致。
	f2, err := fsys.Open(ctx, "/u/u1/.sessions/s1/data/new.txt")
	if err != nil {
		t.Fatal(err)
	}
	f2.Close()

	// 内存层链接 -> ro 区：经链接写被规则表门拒（不是假成功）。
	if err := fsys.Symlink(ctx, "/u/u1", "/proc/rolink"); err != nil {
		t.Fatal(err)
	}
	err = writeFile(t, fsys, "/proc/rolink/x.txt", "evil")
	if !errors.Is(err, ErrRuleDenied) {
		t.Fatalf("write through link to ro = %v, want ErrRuleDenied", err)
	}
	if _, err := backing.Stat("/u/u1/x.txt"); err == nil {
		t.Fatal("ro area must not be written")
	}

	// Remove 作用于链接本身（NoFollow）：链接删除、目标完好。
	if err := fsys.Remove(ctx, "/proc/link", false); err != nil {
		t.Fatal(err)
	}
	if _, err := backing.Stat("/u/u1/.sessions/s1/data/real.txt"); err != nil {
		t.Fatalf("remove link must not touch target: %v", err)
	}
	if _, err := fsys.Lstat(ctx, "/proc/link"); err == nil {
		t.Fatal("link should be removed")
	}
}

// TestCloudFSJailRootMetadataTraversable 回归（2026-10-05）：jail 根（次级根
// /skills）的元数据（Stat/Lstat/Realpath）放行——vsh 对 lstat 语义会 Realpath
// 其父目录做符号链接解析，`ls /skills/{id}`（无尾斜杠）的父是 /skills；列目录
// （ReadDir）仍走 gate 受规则表拒，内容不放宽。
func TestCloudFSJailRootMetadataTraversable(t *testing.T) {
	t.Parallel()
	backing, err := ufs.NewLocalFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rules := vbox.NewFSRuleSet([]vbox.Rule{
		{Pattern: "/u/u1", Effect: vbox.EffRW, Class: vbox.ClassConvenience},
		{Pattern: "/skills/ok", Effect: vbox.EffRO, Class: vbox.ClassCfg},
		{Pattern: "/skills", Effect: vbox.EffDeny, Class: vbox.ClassCfg},
	}, vbox.EffDeny)
	fsys, err := NewCloudFS(CloudFSConfig{
		UserRoot:  "/u/u1",
		JailExtra: []string{"/skills"},
		Backing:   backing,
		Rules:     func() vbox.FSRuleSet { return rules },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := backing.MkdirAll("/skills/ok", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := backing.WriteFile("/skills/ok/SKILL.md", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// jail 根元数据放行（stat/lstat/realpath）。
	if _, err := fsys.Stat(ctx, "/skills"); err != nil {
		t.Fatalf("Stat(/skills) = %v, want nil（jail 根元数据放行）", err)
	}
	if _, err := fsys.Lstat(ctx, "/skills"); err != nil {
		t.Fatalf("Lstat(/skills) = %v, want nil", err)
	}
	if p, err := fsys.Realpath(ctx, "/skills"); err != nil || p != "/skills" {
		t.Fatalf("Realpath(/skills) = %q, %v; want /skills", p, err)
	}
	// 一级子目录（RO 行）可 lstat。
	if _, err := fsys.Lstat(ctx, "/skills/ok"); err != nil {
		t.Fatalf("Lstat(/skills/ok) = %v, want nil", err)
	}
	// 列目录仍拒（不放宽：ReadDir 走 gate）。
	if _, err := fsys.ReadDir(ctx, "/skills"); !errors.Is(err, ErrRuleDenied) {
		t.Fatalf("ReadDir(/skills) = %v, want ErrRuleDenied", err)
	}
	// 他人私有行/保留名内容读仍拒（deny 兜底）。
	if _, err := fsys.Open(ctx, "/skills/other/SKILL.md"); !errors.Is(err, ErrRuleDenied) {
		t.Fatalf("Open other skill = %v, want ErrRuleDenied", err)
	}
}
