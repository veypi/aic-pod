package vcore

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

var testTime = time.Unix(1700000000, 0)

// find 默认上限 100 条：查 limit+1 判定截断，恰好 100 条不误标（§5.4）。
func TestFindLimit(t *testing.T) {
	vfs := NewMemVFS()
	for i := 0; i < 101; i++ {
		vfs.SetFile(fmt.Sprintf("/d/f%03d.txt", i), []byte("x"), testTime)
	}
	env := &Env{VFS: vfs, Workdir: "/d"}

	res, err := Run(context.Background(), env, "find", []string{"/d"})
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
	res2, err := Run(context.Background(), &Env{VFS: vfs2, Workdir: "/d"}, "find", []string{"/d"})
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

// grep 大候选文件流式匹配（>8MB）。
func TestGrepLargeFile(t *testing.T) {
	vfs := NewMemVFS()
	var sb strings.Builder
	for i := 0; i < 200000; i++ {
		fmt.Fprintf(&sb, "line %d padding padding padding padding padding\n", i)
	}
	sb.WriteString("needle here\n")
	vfs.SetFile("/big.log", []byte(sb.String()), testTime)
	env := &Env{VFS: vfs, Workdir: "/"}

	res, err := Run(context.Background(), env, "grep", []string{"needle", "/big.log"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Attrs["rows"] != "1" || !strings.Contains(res.Content, "/big.log:200001\tneedle here") {
		t.Errorf("res = %v %.80q", res.Attrs, res.Content)
	}
}
