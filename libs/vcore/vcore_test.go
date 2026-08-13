package vcore

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

var testTime = time.Unix(1700000000, 0)

// rg files 平台上限 100 条：超限标记 truncated，恰好 100 条不误标（§4.6）。
func TestRgFilesLimit(t *testing.T) {
	vfs := NewMemVFS()
	for i := 0; i < 101; i++ {
		vfs.SetFile(fmt.Sprintf("/d/f%03d.txt", i), []byte("x"), testTime)
	}
	env := &Env{VFS: vfs, Workdir: "/d"}

	res, err := RunFS(context.Background(), env, []byte(`{"action":"rg","files":true,"path":"/d"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Attrs["rows"] != "100" || res.Attrs["truncated"] != "true" {
		t.Errorf("101 files: rows=%s truncated=%s, want 100/true", res.Attrs["rows"], res.Attrs["truncated"])
	}

	vfs2 := NewMemVFS()
	for i := 0; i < 100; i++ {
		vfs2.SetFile(fmt.Sprintf("/d/f%03d.txt", i), []byte("x"), testTime)
	}
	res2, err := RunFS(context.Background(), &Env{VFS: vfs2, Workdir: "/d"}, []byte(`{"action":"rg","files":true,"path":"/d"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res2.Attrs["rows"] != "100" || res2.Attrs["truncated"] != "false" {
		t.Errorf("100 files: rows=%s truncated=%s, want 100/false", res2.Attrs["rows"], res2.Attrs["truncated"])
	}
}

// read 大文件（>8MB）流式路径：总行数精确、窗口截取、truncated 标记（§4.2）。
func TestReadLargeFile(t *testing.T) {
	vfs := NewMemVFS()
	var sb strings.Builder
	for i := 0; i < 200000; i++ { // ~1.4MB/10w 行 × 2 → 超过 8MB
		fmt.Fprintf(&sb, "line %d content padding padding padding padding\n", i)
	}
	data := sb.String()
	if len(data) <= streamThreshold {
		t.Fatalf("test file too small: %d", len(data))
	}
	vfs.SetFile("/big.log", []byte(data), testTime)
	env := &Env{VFS: vfs, Workdir: "/"}

	res, err := RunFS(context.Background(), env, []byte(`{"action":"read","path":"/big.log","offset":199999,"limit":5}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Attrs["total_lines"] != "200000" || res.Attrs["rows"] != "2" || res.Attrs["truncated"] != "false" {
		t.Errorf("attrs = %v", res.Attrs)
	}
	if !strings.HasPrefix(res.Content, "199999\tline 199998") {
		t.Errorf("content head = %.60q", res.Content)
	}

	_, err = RunFS(context.Background(), env, []byte(`{"action":"read","path":"/big.log","offset":200001}`))
	if err == nil || err.Error() != "fs read: offset 200001 exceeds 200000 lines" {
		t.Errorf("offset overflow err = %v", err)
	}
}

// ls 节点上限：超过 lsMaxNodes 标记 truncated 且 rows 截断（§4.5 有界子树）。
func TestLsNodeCap(t *testing.T) {
	vfs := NewMemVFS()
	for i := 0; i < lsMaxNodes+1; i++ {
		vfs.SetFile(fmt.Sprintf("/d/f%05d.txt", i), []byte("x"), testTime)
	}
	env := &Env{VFS: vfs, Workdir: "/d"}

	res, err := RunFS(context.Background(), env, []byte(`{"action":"ls","path":"/d","depth":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Attrs["truncated"] != "true" {
		t.Errorf("truncated = %s, want true", res.Attrs["truncated"])
	}
	if res.Attrs["rows"] != fmt.Sprintf("%d", lsMaxNodes) {
		t.Errorf("rows = %s, want %d", res.Attrs["rows"], lsMaxNodes)
	}
}

// rg 大候选文件流式匹配（>8MB），输出 rg 管道格式（冒号分隔）。
func TestRgLargeFile(t *testing.T) {
	vfs := NewMemVFS()
	var sb strings.Builder
	for i := 0; i < 200000; i++ {
		fmt.Fprintf(&sb, "line %d padding padding padding padding padding\n", i)
	}
	sb.WriteString("needle here\n")
	vfs.SetFile("/big.log", []byte(sb.String()), testTime)
	env := &Env{VFS: vfs, Workdir: "/"}

	res, err := RunFS(context.Background(), env, []byte(`{"action":"rg","pattern":"needle","path":"/big.log"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Attrs["rows"] != "1" || !strings.Contains(res.Content, "/big.log:200001:needle here") {
		t.Errorf("res = %v %.80q", res.Attrs, res.Content)
	}
}

// 安全：Roots 收容（§2.1.1 执行层）——路径越出三根一律 DeniedError，
// 覆盖 fs 全部文件 action 与 exec curl（read/write/edit 同源）。
func TestRootsContainment(t *testing.T) {
	roots := []string{"/home/u1", "/agents/a1", "/sessions/s1"}
	vars := map[string]string{"$USER": "/home/u1", "$AGENT": "/agents/a1", "$SESSION": "/sessions/s1"}
	newEnv := func() *Env {
		vfs := NewMemVFS()
		vfs.SetDir("/sessions/s1", testTime)
		return &Env{
			VFS:     vfs,
			Workdir: "/sessions/s1",
			Vars:    vars,
			Roots:   roots,
		}
	}

	fsCases := []struct {
		name   string
		params string
	}{
		{"rm absolute escape", `{"action":"rm","path":"/etc/passwd"}`},
		{"rm traversal", `{"action":"rm","path":"../../etc/passwd"}`},
		{"write absolute escape", `{"action":"write","path":"/tmp/x","content":"x"}`},
		{"write traversal", `{"action":"write","path":"../../x","content":"x"}`},
		{"cp src escape", `{"action":"cp","src":"/etc/passwd","dst":"/sessions/s1/a.txt"}`},
		{"cp dst escape", `{"action":"cp","src":"/sessions/s1/a.txt","dst":"/etc/a.txt"}`},
		{"mv dst escape", `{"action":"mv","src":"/sessions/s1/a.txt","dst":"/tmp/a.txt"}`},
		{"ls escape", `{"action":"ls","path":"/etc"}`},
	}
	for _, c := range fsCases {
		env := newEnv()
		if _, err := RunFS(context.Background(), env, []byte(c.params)); err == nil {
			t.Errorf("%s: escape accepted, want DeniedError", c.name)
		} else if !strings.Contains(err.Error(), "outside allowed roots") {
			t.Errorf("%s: wrong error: %v", c.name, err)
		}
	}

	// exec curl -o 同源收容
	env := newEnv()
	if _, err := Run(context.Background(), env, "curl", []string{"-o", "/etc/x", "https://example.com"}); err == nil {
		t.Errorf("curl dst escape: escape accepted, want DeniedError")
	} else if !strings.Contains(err.Error(), "outside allowed roots") {
		t.Errorf("curl dst escape: wrong error: %v", err)
	}

	// 根内路径放行
	vfs2 := NewMemVFS()
	vfs2.SetFile("/sessions/s1/a.txt", []byte("x"), testTime)
	env2 := &Env{VFS: vfs2, Workdir: "/sessions/s1", Vars: vars, Roots: roots}
	if _, err := RunFS(context.Background(), env2, []byte(`{"action":"ls","path":"/sessions/s1"}`)); err != nil {
		t.Errorf("in-root ls rejected: %v", err)
	}
}

// 物理 host（Roots=nil）：路径不限制。
func TestRootsNilUnrestricted(t *testing.T) {
	vfs := NewMemVFS()
	vfs.SetFile("/etc/passwd", []byte("x"), testTime)
	env := &Env{VFS: vfs, Workdir: "/workspace"} // Vars=nil Roots=nil
	if _, err := RunFS(context.Background(), env, []byte(`{"action":"ls","path":"/etc"}`)); err != nil {
		t.Errorf("host ls should be unrestricted: %v", err)
	}
}
