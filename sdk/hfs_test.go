package aichost

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func hfsCall(t *testing.T, data map[string]any) *ToolResult {
	t.Helper()
	res, err := handleHfs(context.Background(), data)
	if err != nil {
		t.Fatalf("handleHfs error: %v", err)
	}
	if res == nil {
		t.Fatal("handleHfs returned nil result")
	}
	return res
}

func hfsOK(t *testing.T, data map[string]any) *ToolResult {
	t.Helper()
	res := hfsCall(t, data)
	if res.Error != "" {
		t.Fatalf("hfs %v failed: %s", data["action"], res.Error)
	}
	if res.State == "rejected" {
		t.Fatalf("hfs %v rejected: %s", data["action"], res.Error)
	}
	return res
}

func TestHfsWriteReadStatLs(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "sub", "hello.txt")
	content := "line1\nline2\n"

	// write（自动创建父目录）
	res := hfsOK(t, map[string]any{
		"action": "write", "path": file, "truncate": true,
		"data_b64": base64.StdEncoding.EncodeToString([]byte(content)),
	})
	if res.Attrs["bytes"] != "12" {
		t.Fatalf("write bytes = %q, want 12", res.Attrs["bytes"])
	}

	// stat
	res = hfsOK(t, map[string]any{"action": "stat", "path": file})
	var st struct {
		Exists bool   `json:"exists"`
		Dir    bool   `json:"dir"`
		Size   int64  `json:"size"`
		Mime   string `json:"mime"`
	}
	if err := json.Unmarshal([]byte(res.Content), &st); err != nil {
		t.Fatalf("stat json: %v", err)
	}
	if !st.Exists || st.Dir || st.Size != 12 {
		t.Fatalf("stat = %+v, want exists file size 12", st)
	}

	// stat 不存在路径 → exists:false（非错误）
	res = hfsOK(t, map[string]any{"action": "stat", "path": filepath.Join(dir, "nope")})
	var st2 struct {
		Exists bool `json:"exists"`
	}
	_ = json.Unmarshal([]byte(res.Content), &st2)
	if st2.Exists {
		t.Fatal("stat on missing path: exists should be false")
	}

	// read 全量（单块）
	res = hfsOK(t, map[string]any{"action": "read", "path": file, "offset": 0, "length": 1 << 20})
	got, _ := base64.StdEncoding.DecodeString(res.Content)
	if string(got) != content {
		t.Fatalf("read = %q, want %q", got, content)
	}
	if res.Attrs["eof"] != "true" || res.Attrs["size"] != "12" {
		t.Fatalf("read attrs = %v", res.Attrs)
	}
	if !strings.HasPrefix(res.Attrs["mime"], "text/") {
		t.Fatalf("read mime = %q, want text/*", res.Attrs["mime"])
	}

	// read 分块：offset=6 应得 "line2\n"
	res = hfsOK(t, map[string]any{"action": "read", "path": file, "offset": 6, "length": 1 << 20})
	got, _ = base64.StdEncoding.DecodeString(res.Content)
	if string(got) != "line2\n" {
		t.Fatalf("read chunk = %q, want %q", got, "line2\n")
	}
	if res.Attrs["mime"] != "" {
		t.Fatalf("read non-first chunk mime = %q, want empty", res.Attrs["mime"])
	}

	// ls 目录
	res = hfsOK(t, map[string]any{"action": "ls", "path": dir})
	var listing hfsItem
	if err := json.Unmarshal([]byte(res.Content), &listing); err != nil {
		t.Fatalf("ls json: %v", err)
	}
	if !listing.Dir || len(listing.Items) != 1 || listing.Items[0].Name != "sub" || !listing.Items[0].Dir {
		t.Fatalf("ls = %+v", listing)
	}

	// ls 文件 → 单条目无 items
	res = hfsOK(t, map[string]any{"action": "ls", "path": file})
	var single hfsItem
	_ = json.Unmarshal([]byte(res.Content), &single)
	if single.Dir || len(single.Items) != 0 {
		t.Fatalf("ls file = %+v", single)
	}
}

func TestHfsWriteChunked(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "big.bin")
	part1 := []byte("AAAA-part1")
	part2 := []byte("-BBBB-part2")

	hfsOK(t, map[string]any{"action": "write", "path": file, "offset": 0, "truncate": true,
		"data_b64": base64.StdEncoding.EncodeToString(part1)})
	hfsOK(t, map[string]any{"action": "write", "path": file, "offset": int64(len(part1)),
		"data_b64": base64.StdEncoding.EncodeToString(part2)})

	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(part1)+string(part2) {
		t.Fatalf("chunked write = %q", data)
	}
}

func TestHfsCpMvRmMkdir(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.txt")
	dst := filepath.Join(dir, "b.txt")
	mvd := filepath.Join(dir, "c.txt")
	os.WriteFile(src, []byte("x"), 0o644)

	// cp
	hfsOK(t, map[string]any{"action": "cp", "path": src, "to": dst})
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("cp: dst missing: %v", err)
	}
	// cp 目标已存在 → 错误
	res := hfsCall(t, map[string]any{"action": "cp", "path": src, "to": dst})
	if res.Error == "" || !strings.Contains(res.Error, "already exists") {
		t.Fatalf("cp existing dst should fail, got %+v", res)
	}
	// mv
	hfsOK(t, map[string]any{"action": "mv", "path": dst, "to": mvd})
	if _, err := os.Stat(mvd); err != nil {
		t.Fatalf("mv: dst missing: %v", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatal("mv: src should be gone")
	}
	// mv 目标已存在 → 错误
	res = hfsCall(t, map[string]any{"action": "mv", "path": src, "to": mvd})
	if res.Error == "" || !strings.Contains(res.Error, "already exists") {
		t.Fatalf("mv existing dst should fail, got %+v", res)
	}
	// mkdir
	sub := filepath.Join(dir, "d1", "d2")
	hfsOK(t, map[string]any{"action": "mkdir", "path": sub})
	if info, err := os.Stat(sub); err != nil || !info.IsDir() {
		t.Fatalf("mkdir: %v", err)
	}
	// mkdir 已存在 → 错误
	res = hfsCall(t, map[string]any{"action": "mkdir", "path": sub})
	if res.Error == "" {
		t.Fatal("mkdir existing should fail")
	}
	// rm
	hfsOK(t, map[string]any{"action": "rm", "path": mvd})
	if _, err := os.Stat(mvd); !os.IsNotExist(err) {
		t.Fatal("rm: file should be gone")
	}
	// rm 根目录 → rejected
	res = hfsCall(t, map[string]any{"action": "rm", "path": "/"})
	if res.State != "rejected" {
		t.Fatalf("rm root should be rejected, got %+v", res)
	}
}

func TestHfsSearch(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "one.md"), []byte("hello\nworld\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "two.txt"), []byte("hello again\n"), 0o644)
	os.Mkdir(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "sub", "three.md"), []byte("nothing\n"), 0o644)

	// glob 文件名搜索
	res := hfsOK(t, map[string]any{"action": "search", "path": dir, "glob": "*.md"})
	var rows []hfsMatch
	if err := json.Unmarshal([]byte(res.Content), &rows); err != nil {
		t.Fatalf("search json: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("glob search rows = %d, want 2 (%+v)", len(rows), rows)
	}
	for _, r := range rows {
		if r.IsDir || !strings.HasSuffix(r.Path, ".md") {
			t.Fatalf("unexpected row: %+v", r)
		}
	}

	// pattern 行搜索
	res = hfsOK(t, map[string]any{"action": "search", "path": dir, "pattern": "*hello*"})
	rows = nil
	_ = json.Unmarshal([]byte(res.Content), &rows)
	if len(rows) != 2 {
		t.Fatalf("pattern search rows = %d, want 2 (%+v)", len(rows), rows)
	}
	if rows[0].LineNum <= 0 || rows[0].Line == "" {
		t.Fatalf("grep row missing line info: %+v", rows[0])
	}

	// 无 glob/pattern → 错误
	res = hfsCall(t, map[string]any{"action": "search", "path": dir})
	if res.Error == "" {
		t.Fatal("search without glob/pattern should fail")
	}
}

func TestHfsNormPath(t *testing.T) {
	home, _ := os.UserHomeDir()
	if got := hfsNormPath("~"); got != home {
		t.Fatalf("hfsNormPath(~) = %q, want %q", got, home)
	}
	if got := hfsNormPath("~/x"); !strings.HasPrefix(got, home) {
		t.Fatalf("hfsNormPath(~/x) = %q", got)
	}
	// 相对路径 Clean 后不变（由调用方保证绝对路径）
	if got := hfsNormPath("  /tmp//a/../b  "); got != "/tmp/b" {
		t.Fatalf("hfsNormPath clean = %q, want /tmp/b", got)
	}
}

func TestHfsUnknownAction(t *testing.T) {
	res := hfsCall(t, map[string]any{"action": "exec", "path": "/tmp"})
	if res.Error == "" || !strings.Contains(res.Error, "unknown action") {
		t.Fatalf("unknown action should fail, got %+v", res)
	}
	res = hfsCall(t, map[string]any{"action": "read"})
	if res.Error == "" {
		t.Fatal("read without path should fail")
	}
}

// TestUnsignedToolFlow 验证免签工具在 handleToolRequest 全流程中的行为：
// hfs 无签名 → 通过；fs（签名工具）无签名 → 拒绝。
func TestUnsignedToolFlow(t *testing.T) {
	c := &Client{tools: []Tool{HfsTool(), FsTool()}}
	if hfs := c.findTool("hfs"); hfs == nil || !hfs.Unsigned {
		t.Fatal("hfs tool should be registered as Unsigned")
	}
	if fs := c.findTool("fs"); fs == nil || fs.Unsigned {
		t.Fatal("fs tool should require signature")
	}
	if unsignedToolGrantedLevel != 3 {
		t.Fatalf("unsignedToolGrantedLevel = %d, want 3", unsignedToolGrantedLevel)
	}
}
