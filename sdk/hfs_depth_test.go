package aichost

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// ls 的 depth 递归语义与 cwd 属性回归测试（前端文件树缓存依赖两者）。
func TestHfsLsDepthAndCwd(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a", "b", "c.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "top.md"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}

	// depth=1：只列直接子项，目录不带 items
	res, err := hfsLs(hfsRequest{Path: root, Depth: 1})
	if err != nil {
		t.Fatal(err)
	}
	var n hfsItem
	if err := json.Unmarshal([]byte(res.Content), &n); err != nil {
		t.Fatal(err)
	}
	if len(n.Items) != 2 {
		t.Fatalf("depth1 items = %d, want 2", len(n.Items))
	}
	for _, it := range n.Items {
		if it.Dir && it.Items != nil {
			t.Fatalf("depth1: dir %s has nested items", it.Name)
		}
	}
	if res.Attrs["cwd"] == "" {
		t.Fatal("missing cwd attr")
	}

	// depth=3：可嵌套到 c.txt
	res, err = hfsLs(hfsRequest{Path: root, Depth: 3})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(res.Content), &n); err != nil {
		t.Fatal(err)
	}
	var a *hfsItem
	for i := range n.Items {
		if n.Items[i].Name == "a" {
			a = &n.Items[i]
		}
	}
	if a == nil || len(a.Items) != 1 || a.Items[0].Name != "b" ||
		len(a.Items[0].Items) != 1 || a.Items[0].Items[0].Name != "c.txt" {
		t.Fatalf("depth3 nesting wrong: %+v", a)
	}
}
