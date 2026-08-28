package vcore

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

var testTime = time.Unix(1700000000, 0)

// rg 列举模式（pattern 缺省）平台上限 50 条：超限标记 truncated，恰好 50 条不误标（§4.6）。
func TestRgFilesLimit(t *testing.T) {
	vfs := NewMemVFS()
	for i := 0; i < 51; i++ {
		vfs.SetFile(fmt.Sprintf("/d/f%02d.txt", i), []byte("x"), testTime)
	}
	env := &Env{VFS: vfs, Workdir: "/d"}

	res, err := RunFS(context.Background(), env, []byte(`{"action":"rg","path":"/d"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Attrs["rows"] != "50" || res.Attrs["truncated"] != "true" {
		t.Errorf("51 files: rows=%s truncated=%s, want 50/true", res.Attrs["rows"], res.Attrs["truncated"])
	}

	vfs2 := NewMemVFS()
	for i := 0; i < 50; i++ {
		vfs2.SetFile(fmt.Sprintf("/d/f%02d.txt", i), []byte("x"), testTime)
	}
	res2, err := RunFS(context.Background(), &Env{VFS: vfs2, Workdir: "/d"}, []byte(`{"action":"rg","path":"/d"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res2.Attrs["rows"] != "50" || res2.Attrs["truncated"] != "false" {
		t.Errorf("50 files: rows=%s truncated=%s, want 50/false", res2.Attrs["rows"], res2.Attrs["truncated"])
	}

	// limit 覆盖默认上限：51 文件 limit=100 → 全量不截断
	res3, err := RunFS(context.Background(), env, []byte(`{"action":"rg","path":"/d","limit":100}`))
	if err != nil {
		t.Fatal(err)
	}
	if res3.Attrs["rows"] != "51" || res3.Attrs["truncated"] != "false" {
		t.Errorf("51 files limit=100: rows=%s truncated=%s, want 51/false", res3.Attrs["rows"], res3.Attrs["truncated"])
	}
}

// rg 超长单行（minified 文件）命中：内容 rune 安全截断 + truncated 标记，
// 单行 text 不超过 rgMaxLineBytes，总 Content 不突破 MaxContentBytes（§2.5）。
func TestRgLongLineClip(t *testing.T) {
	vfs := NewMemVFS()
	long := strings.Repeat("A", 700<<10) + "foo" + strings.Repeat("B", 100) // ~700KB 单行
	vfs.SetFile("/big.min.js", []byte(long), testTime)
	env := &Env{VFS: vfs, Workdir: "/"}

	res, err := RunFS(context.Background(), env, []byte(`{"action":"rg","pattern":"foo","path":"/big.min.js"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Content) > MaxContentBytes {
		t.Fatalf("content %d bytes exceeds budget %d", len(res.Content), MaxContentBytes)
	}
	// content 为结构化 JSON：解析后检查命中行 text 被 4KB 截断
	var out struct {
		Files []struct {
			Matches []struct {
				Line int    `json:"line"`
				Text string `json:"text"`
			} `json:"matches"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatalf("content not valid JSON: %v (%.80q...)", err, res.Content)
	}
	if len(out.Files) != 1 || len(out.Files[0].Matches) != 1 {
		t.Fatalf("unexpected shape: %+v", out)
	}
	m := out.Files[0].Matches[0]
	if !strings.HasSuffix(m.Text, "...[truncated]") {
		t.Errorf("text not clipped: %.100q...", m.Text)
	}
	if len(m.Text) > rgMaxLineBytes+len("...[truncated]") {
		t.Errorf("text %d bytes exceeds line cap", len(m.Text))
	}
	if res.Attrs["rows"] != "1" || res.Attrs["truncated"] != "true" {
		t.Errorf("attrs = %v, want rows=1 truncated=true", res.Attrs)
	}
}

// rg minified 文件默认跳过（采样 64KB 内换行 < 32）：目录递归时不计入结果
// 并置 attrs.skipped；all=true 收录；显式单文件路径不跳过（§4.6）。
func TestRgMinifiedSkip(t *testing.T) {
	bundle := strings.Repeat("AB", 20000) + "foo" + strings.Repeat("CD", 100) // ~40KB 单行（≥ minifiedMinBytes）
	vfs := NewMemVFS()
	vfs.SetFile("/d/a.txt", []byte("foo\n"), testTime)
	vfs.SetFile("/d/bundle.js", []byte(bundle), testTime)
	env := &Env{VFS: vfs, Workdir: "/d"}

	// 默认：minified 跳过，skipped 计数
	res, err := RunFS(context.Background(), env, []byte(`{"action":"rg","pattern":"foo","path":"/d"}`))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"files":[{"path":"/d/a.txt","matches":[{"line":1,"text":"foo"}]}],"note":"1 minified files skipped, use all=true to include"}`
	if res.Content != want {
		t.Errorf("content = %q, want %q", res.Content, want)
	}
	if res.Attrs["skipped"] != "1" || res.Attrs["rows"] != "1" {
		t.Errorf("attrs = %v, want skipped=1 rows=1", res.Attrs)
	}

	// all=true：收录 minified
	res2, err := RunFS(context.Background(), env, []byte(`{"action":"rg","pattern":"foo","path":"/d","all":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res2.Content, `"/d/bundle.js"`) {
		t.Errorf("all=true: bundle.js missing from content: %.80q...", res2.Content)
	}
	if _, ok := res2.Attrs["skipped"]; ok {
		t.Errorf("all=true: unexpected skipped attr: %v", res2.Attrs)
	}

	// 显式单文件路径：不跳过（minified 也搜，走 4KB 截断）
	res3, err := RunFS(context.Background(), env, []byte(`{"action":"rg","pattern":"foo","path":"/d/bundle.js"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res3.Content, `{"files":[{"path":"/d/bundle.js","matches":[{"line":1,"text":"ABAB`) {
		t.Errorf("explicit file: content = %.60q..., want bundle.js hit", res3.Content)
	}
	if _, ok := res3.Attrs["skipped"]; ok {
		t.Errorf("explicit file: unexpected skipped attr: %v", res3.Attrs)
	}

	// 无匹配 + 有跳过：空数组 + note 提示逃生通道
	res4, err := RunFS(context.Background(), env, []byte(`{"action":"rg","pattern":"zzz","path":"/d"}`))
	if err != nil {
		t.Fatal(err)
	}
	want4 := `{"files":[],"note":"1 minified files skipped, use all=true to include"}`
	if res4.Content != want4 {
		t.Errorf("no-match content = %q, want %q", res4.Content, want4)
	}
	if res4.Attrs["skipped"] != "1" {
		t.Errorf("no-match attrs = %v, want skipped=1", res4.Attrs)
	}
}

// rg JSON 输出在 128KB 字节预算截断下恒为合法 JSON（§4.6 结构化输出核心不变量）：
// 多文件累计超预算时按文件级原子写入截断，输出仍可 JSON.parse。
func TestRgJSONValidUnderBudget(t *testing.T) {
	vfs := NewMemVFS()
	for i := 0; i < 10; i++ {
		var sb strings.Builder
		for j := 0; j < 20; j++ {
			sb.WriteString("foo " + strings.Repeat("x", 1000) + "\n") // ~1KB/行,20 行/文件 ≈ 20KB
		}
		vfs.SetFile(fmt.Sprintf("/d/f%d.txt", i), []byte(sb.String()), testTime)
	}
	env := &Env{VFS: vfs, Workdir: "/d"}

	res, err := RunFS(context.Background(), env, []byte(`{"action":"rg","pattern":"foo","path":"/d","limit":200}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Content) > MaxContentBytes {
		t.Fatalf("content %d bytes exceeds budget %d", len(res.Content), MaxContentBytes)
	}
	var out struct {
		Files []struct {
			Path    string `json:"path"`
			Matches []struct {
				Line int    `json:"line"`
				Text string `json:"text"`
			} `json:"matches"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatalf("content not valid JSON under budget pressure: %v (%.120q...)", err, res.Content)
	}
	if len(out.Files) == 0 || len(out.Files) >= 10 {
		t.Errorf("files = %d, want 1..9 (budget-truncated)", len(out.Files))
	}
	if res.Attrs["truncated"] != "true" {
		t.Errorf("attrs = %v, want truncated=true", res.Attrs)
	}
	if got := len(out.Files) * 20; res.Attrs["rows"] != fmt.Sprint(got) {
		t.Errorf("rows = %s, want %d (full files only)", res.Attrs["rows"], got)
	}
}

// rg minified 兜底判定（§4.6）：头部含多行 license 注释使采样 64KB 内换行 ≥32
// （采样快路径不命中）但全文件平均行长大的压缩文件（如 echarts.min.js 1MB/45 行）
// 仍判 minified 跳过。
func TestRgMinifiedAvgLine(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 40; i++ {
		sb.WriteString("// license line " + strconv.Itoa(i) + "\n")
	}
	for i := 0; i < 10; i++ {
		sb.WriteString(strings.Repeat("ABCDEFGH", 1200)) // ~9.6KB/行
		sb.WriteString("\n")
	}
	bundle := sb.String() // ~96.7KB，采样内 46 换行 ≥32，全文件平均 ~2.1KB/行
	vfs := NewMemVFS()
	vfs.SetFile("/d/a.txt", []byte("foo\n"), testTime)
	vfs.SetFile("/d/echarts-like.js", []byte(bundle), testTime)
	env := &Env{VFS: vfs, Workdir: "/d"}

	res, err := RunFS(context.Background(), env, []byte(`{"action":"rg","pattern":"foo","path":"/d"}`))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"files":[{"path":"/d/a.txt","matches":[{"line":1,"text":"foo"}]}],"note":"1 minified files skipped, use all=true to include"}`
	if res.Content != want {
		t.Errorf("content = %q, want %q", res.Content, want)
	}
	if res.Attrs["skipped"] != "1" || res.Attrs["rows"] != "1" {
		t.Errorf("attrs = %v, want skipped=1 rows=1", res.Attrs)
	}

	// 对照：同样头部但行数多的正常文件（平均行长 < 1KB）不跳过
	var sb2 strings.Builder
	for i := 0; i < 40; i++ {
		sb2.WriteString("// license line " + strconv.Itoa(i) + "\n")
	}
	for i := 0; i < 4000; i++ {
		sb2.WriteString("const v" + strconv.Itoa(i) + " = " + strconv.Itoa(i) + ";\n") // ~20B/行
	}
	vfs2 := NewMemVFS()
	vfs2.SetFile("/d/ok.js", []byte(sb2.String()), testTime)
	res2, err := RunFS(context.Background(), &Env{VFS: vfs2, Workdir: "/d"}, []byte(`{"action":"rg","pattern":"const v0","path":"/d"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res2.Attrs["skipped"]; ok {
		t.Errorf("normal file skipped: %v", res2.Attrs)
	}
	if res2.Attrs["rows"] != "1" {
		t.Errorf("rows = %v, want 1", res2.Attrs["rows"])
	}
}

// rg minified 命名约定排除（§4.6）：*.min.js/*.min.css/*.min.mjs 后缀即压缩
// 语义，零 I/O 判定且优先于 size（<32KB 也跳过）；显式单文件路径不跳过。
func TestRgMinifiedName(t *testing.T) {
	vfs := NewMemVFS()
	vfs.SetFile("/d/a.txt", []byte("foo\n"), testTime)
	vfs.SetFile("/d/vendor.min.js", []byte("small normal-ish content\nfoo\n"), testTime) // <32KB
	vfs.SetFile("/d/theme.min.css", []byte(".a{color:red}\n"), testTime)
	env := &Env{VFS: vfs, Workdir: "/d"}

	res, err := RunFS(context.Background(), env, []byte(`{"action":"rg","pattern":"foo","path":"/d"}`))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"files":[{"path":"/d/a.txt","matches":[{"line":1,"text":"foo"}]}],"note":"2 minified files skipped, use all=true to include"}`
	if res.Content != want {
		t.Errorf("content = %q, want %q", res.Content, want)
	}
	if res.Attrs["skipped"] != "2" || res.Attrs["rows"] != "1" {
		t.Errorf("attrs = %v, want skipped=2 rows=1", res.Attrs)
	}

	// 显式单文件路径不跳过（与内容判定一致：includeMinified=true）
	res2, err := RunFS(context.Background(), env, []byte(`{"action":"rg","pattern":"foo","path":"/d/vendor.min.js"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res2.Content, "vendor.min.js") {
		t.Errorf("explicit .min.js path should be searched: %.60q", res2.Content)
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

	// 截断时 hint 给出下一页 offset（弱模型兜底，§4.2）
	res2, err := RunFS(context.Background(), env, []byte(`{"action":"read","path":"/big.log","limit":3}`))
	if err != nil {
		t.Fatal(err)
	}
	wantHint := "file has 200000 lines; this call returned 1-3; pass offset=4 to continue reading"
	if res2.Attrs["hint"] != wantHint {
		t.Errorf("hint = %q, want %q", res2.Attrs["hint"], wantHint)
	}

	_, err = RunFS(context.Background(), env, []byte(`{"action":"read","path":"/big.log","offset":200001}`))
	if err == nil || err.Error() != "fs read: offset 200001 exceeds 200000 lines" {
		t.Errorf("offset overflow err = %v", err)
	}
}

// read 截断时 hint attr 显式提示下一页 offset（§4.2）；未截断不设置。
func TestReadTruncatedHint(t *testing.T) {
	vfs := NewMemVFS()
	var sb strings.Builder
	for i := 0; i < 10; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	vfs.SetFile("/a.txt", []byte(sb.String()), testTime)
	env := &Env{VFS: vfs, Workdir: "/"}

	res, err := RunFS(context.Background(), env, []byte(`{"action":"read","path":"/a.txt","limit":4}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Attrs["truncated"] != "true" || res.Attrs["range"] != "1-4" {
		t.Fatalf("attrs = %v", res.Attrs)
	}
	want := "file has 10 lines; this call returned 1-4; pass offset=5 to continue reading"
	if res.Attrs["hint"] != want {
		t.Errorf("hint = %q, want %q", res.Attrs["hint"], want)
	}

	res2, err := RunFS(context.Background(), env, []byte(`{"action":"read","path":"/a.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res2.Attrs["truncated"] != "false" {
		t.Fatalf("attrs = %v", res2.Attrs)
	}
	if _, ok := res2.Attrs["hint"]; ok {
		t.Errorf("hint should be absent when not truncated: %v", res2.Attrs)
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

// rg 大候选文件流式匹配（>8MB），输出结构化 JSON（§4.6）。
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
	if res.Attrs["rows"] != "1" || !strings.Contains(res.Content, `"line":200001,"text":"needle here"`) {
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
