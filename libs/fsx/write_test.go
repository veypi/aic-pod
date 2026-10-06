package fsx

// write_test.go — write/edit 结构化返回与失败诊断（v3）单测。
// 环境 = ufs.NewLocalFS(t.TempDir())；断言 Content 为 JSON 文档
// （成功极简 / 失败详尽），attrs 仅保留 UI 消费键。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/veypi/vigo/contrib/ufs"
)

func newTestEnv(t *testing.T) *Env {
	t.Helper()
	back, err := ufs.NewLocalFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &Env{FS: back, Workdir: "/"}
}

func runFS(t *testing.T, env *Env, args map[string]any) *Result {
	t.Helper()
	raw, _ := json.Marshal(args)
	r, err := RunFS(context.Background(), env, raw)
	if err != nil {
		t.Fatalf("%v: %v", args["action"], err)
	}
	return r
}

func runWrite(t *testing.T, env *Env, path, content string) *Result {
	t.Helper()
	return runFS(t, env, map[string]any{"action": "write", "path": path, "content": content})
}

func runEdit(t *testing.T, env *Env, path string, edits ...map[string]string) *Result {
	t.Helper()
	return runFS(t, env, map[string]any{"action": "edit", "path": path, "edits": edits})
}

func parseOutcome(t *testing.T, r *Result) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(r.Content), &m); err != nil {
		t.Fatalf("content 不是 JSON: %q: %v", r.Content, err)
	}
	return m
}

func errorsOf(t *testing.T, m map[string]any) []map[string]any {
	t.Helper()
	raw, ok := m["errors"].([]any)
	if !ok {
		t.Fatalf("缺 errors 数组: %v", m)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		out = append(out, e.(map[string]any))
	}
	return out
}

func readFile(t *testing.T, env *Env, path string) string {
	t.Helper()
	data, err := env.FS.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestFsWriteStructuredResult(t *testing.T) {
	env := newTestEnv(t)
	r := runWrite(t, env, "/a.txt", "hello\nworld\n")
	m := parseOutcome(t, r)
	if m["ok"] != true || m["lines"] != 2.0 || m["bytes"] != 12.0 {
		t.Fatalf("write 结果字段不对: %v", m)
	}
	if m["v"] == "" {
		t.Fatalf("write 结果缺 v: %v", m)
	}
	if _, hasPath := m["path"]; hasPath {
		t.Fatalf("结果不应回显 path: %v", m)
	}
	// attrs 只保留 UI 消费键
	if r.Attrs["lines"] != "2" || r.Attrs["ok"] != "true" {
		t.Fatalf("attrs 不对: %v", r.Attrs)
	}
	if _, hasPath := r.Attrs["path"]; hasPath {
		t.Fatalf("attrs 不应带 path: %v", r.Attrs)
	}
}

func TestFsEditSuccessMinimal(t *testing.T) {
	env := newTestEnv(t)
	runWrite(t, env, "/e.txt", "alpha beta gamma\n")
	r := runEdit(t, env, "/e.txt",
		map[string]string{"oldText": "beta", "newText": "BETA"},
		map[string]string{"oldText": "gamma", "newText": "GAMMA"},
	)
	m := parseOutcome(t, r)
	if m["ok"] != true || m["applied"] != 2.0 || m["v"] == "" {
		t.Fatalf("成功结果不对: %v", m)
	}
	// 成功极简：无 errors / path
	if _, has := m["errors"]; has {
		t.Fatalf("全成功不应带 errors: %v", m)
	}
	if _, has := m["path"]; has {
		t.Fatalf("全成功不应回显 path: %v", m)
	}
	if r.Attrs["ok"] != "true" || r.Attrs["edits"] != "2" {
		t.Fatalf("attrs 不对: %v", r.Attrs)
	}
	if _, has := r.Attrs["edits_failed"]; has {
		t.Fatalf("全成功不应带 edits_failed: %v", r.Attrs)
	}
	if got := readFile(t, env, "/e.txt"); got != "alpha BETA GAMMA\n" {
		t.Fatalf("文件内容: %q", got)
	}
}

func TestFsEditChained(t *testing.T) {
	// 链式：后一条锚依赖前一条的输出（顺序语义保留）
	env := newTestEnv(t)
	runWrite(t, env, "/e.txt", "start\nend\n")
	r := runEdit(t, env, "/e.txt",
		map[string]string{"oldText": "start\n", "newText": "MARKER\n"},
		map[string]string{"oldText": "MARKER\nend", "newText": "MARKER\nmid\nend"},
	)
	m := parseOutcome(t, r)
	if m["ok"] != true || m["applied"] != 2.0 {
		t.Fatalf("链式编辑失败: %v", m)
	}
	if got := readFile(t, env, "/e.txt"); got != "MARKER\nmid\nend\n" {
		t.Fatalf("文件内容: %q", got)
	}
}

func TestFsEditPartialFailure(t *testing.T) {
	env := newTestEnv(t)
	runWrite(t, env, "/e.txt", "alpha beta gamma\n")
	r := runEdit(t, env, "/e.txt",
		map[string]string{"oldText": "beta", "newText": "BETA"},
		map[string]string{"oldText": "nothing-here-xxx", "newText": "y"},
	)
	m := parseOutcome(t, r)
	if m["ok"] != false || m["applied"] != 1.0 || m["v"] == "" {
		t.Fatalf("部分失败结果不对: %v", m)
	}
	errs := errorsOf(t, m)
	if len(errs) != 1 || errs[0]["i"] != 2.0 || errs[0]["reason"] != "not-found" {
		t.Fatalf("errors 不对: %v", errs)
	}
	if r.Attrs["ok"] != "false" || r.Attrs["edits"] != "1" || r.Attrs["edits_failed"] != "1" {
		t.Fatalf("attrs 不对: %v", r.Attrs)
	}
	// 成功条目已落盘
	if got := readFile(t, env, "/e.txt"); got != "alpha BETA gamma\n" {
		t.Fatalf("文件内容: %q", got)
	}
}

func TestFsEditAllFailedNoError(t *testing.T) {
	env := newTestEnv(t)
	runWrite(t, env, "/e.txt", "alpha beta\n")
	// 全失败（含单条失败）：不走 error 通道，返回 ok:false，不写盘
	r := runEdit(t, env, "/e.txt", map[string]string{"oldText": "nothing-here-xxx", "newText": "y"})
	m := parseOutcome(t, r)
	if m["ok"] != false || m["applied"] != 0.0 {
		t.Fatalf("全失败结果不对: %v", m)
	}
	if _, has := m["v"]; has {
		t.Fatalf("未落盘不应带 v: %v", m)
	}
	errs := errorsOf(t, m)
	if len(errs) != 1 || errs[0]["reason"] != "not-found" {
		t.Fatalf("errors 不对: %v", errs)
	}
	if got := readFile(t, env, "/e.txt"); got != "alpha beta\n" {
		t.Fatalf("全失败不应写盘: %q", got)
	}
}

func TestFsEditAmbiguousMatches(t *testing.T) {
	env := newTestEnv(t)
	runWrite(t, env, "/e.txt", "alpha beta alpha\ngamma\nalpha delta\n")
	r := runEdit(t, env, "/e.txt", map[string]string{"oldText": "alpha", "newText": "x"})
	m := parseOutcome(t, r)
	errs := errorsOf(t, m)
	if len(errs) != 1 || errs[0]["reason"] != "ambiguous" {
		t.Fatalf("应为 ambiguous: %v", errs)
	}
	raw, _ := json.Marshal(errs[0]["matches"])
	if string(raw) != "[1,1,3]" {
		t.Fatalf("matches 行号不对: %s", raw)
	}
}

func TestFsEditNearestNormalized(t *testing.T) {
	// 智能引号漂移：文件是 “hello” ，锚用直引号——归一化后命中
	env := newTestEnv(t)
	runWrite(t, env, "/e.txt", "package main\n\nfunc greet() string {\n\treturn “hello” + name\n}\n")
	r := runEdit(t, env, "/e.txt",
		map[string]string{"oldText": "\treturn \"hello\" + name", "newText": "\treturn \"hi\" + name"})
	m := parseOutcome(t, r)
	errs := errorsOf(t, m)
	if len(errs) != 1 || errs[0]["reason"] != "not-found" {
		t.Fatalf("应为 not-found: %v", errs)
	}
	nearest, ok := errs[0]["nearest"].(map[string]any)
	if !ok {
		t.Fatalf("缺 nearest: %v", errs[0])
	}
	if nearest["kind"] != "normalized" || nearest["line"] != 4.0 {
		t.Fatalf("nearest 不对: %v", nearest)
	}
	if !strings.Contains(nearest["text"].(string), "“hello”") {
		t.Fatalf("nearest.text 应含真实文本: %v", nearest["text"])
	}
}

func TestFsEditNearestSimilar(t *testing.T) {
	// 内容偏差（非纯空白漂移）：归一化不命中，行级相似度顶上
	env := newTestEnv(t)
	runWrite(t, env, "/e.txt", "package main\n\nfunc compute() int {\n\treturn holdDip(value)\n}\n")
	r := runEdit(t, env, "/e.txt",
		map[string]string{"oldText": "\treturn holdDip(vale)\n}", "newText": "\treturn 0\n}"})
	m := parseOutcome(t, r)
	errs := errorsOf(t, m)
	if len(errs) != 1 || errs[0]["reason"] != "not-found" {
		t.Fatalf("应为 not-found: %v", errs)
	}
	nearest, ok := errs[0]["nearest"].(map[string]any)
	if !ok || nearest["kind"] != "similar" {
		t.Fatalf("应为 similar nearest: %v", errs[0])
	}
	if nearest["line"] != 4.0 {
		t.Fatalf("similar 行号不对: %v", nearest)
	}
	if sim, _ := nearest["similarity"].(float64); sim < 0.6 {
		t.Fatalf("similarity 过低: %v", nearest)
	}
}

func TestFsEditShortAnchorSkipsNearest(t *testing.T) {
	env := newTestEnv(t)
	runWrite(t, env, "/e.txt", "alpha beta gamma delta\n")
	r := runEdit(t, env, "/e.txt", map[string]string{"oldText": "x", "newText": "y"})
	m := parseOutcome(t, r)
	errs := errorsOf(t, m)
	if len(errs) != 1 {
		t.Fatalf("errors 不对: %v", errs)
	}
	if _, has := errs[0]["nearest"]; has {
		t.Fatalf("短锚不应给 nearest: %v", errs[0])
	}
}

func TestFsEditParamReasons(t *testing.T) {
	env := newTestEnv(t)
	runWrite(t, env, "/e.txt", "alpha\n")
	r := runEdit(t, env, "/e.txt",
		map[string]string{"oldText": "", "newText": "y"},
		map[string]string{"oldText": "alpha", "newText": "alpha"},
	)
	m := parseOutcome(t, r)
	errs := errorsOf(t, m)
	if len(errs) != 2 || errs[0]["reason"] != "empty-oldText" || errs[1]["reason"] != "identical-old-new" {
		t.Fatalf("reason 不对: %v", errs)
	}
}

func TestFsEditDoubleEncodingHint(t *testing.T) {
	env := newTestEnv(t)
	runWrite(t, env, "/e.txt", "<div class=\"box\">content here</div>\n")
	// oldText 含字面 ＼u003c 转义（双重编码残留），解码形式在文件中存在
	r := runEdit(t, env, "/e.txt",
		map[string]string{"oldText": `\u003cdiv class="box"\u003e`, "newText": "x"})
	m := parseOutcome(t, r)
	errs := errorsOf(t, m)
	if len(errs) != 1 || errs[0]["reason"] != "not-found" {
		t.Fatalf("应为 not-found: %v", errs)
	}
	hint, _ := errs[0]["hint"].(string)
	if !strings.Contains(hint, "double JSON encoding") {
		t.Fatalf("缺双重编码 hint: %v", errs[0])
	}
}

func TestFsEditOperationalErrors(t *testing.T) {
	env := newTestEnv(t)
	// 文件不存在：操作性失败走 error 通道
	raw, _ := json.Marshal(map[string]any{"action": "edit", "path": "/missing.txt",
		"edits": []map[string]string{{"oldText": "a", "newText": "b"}}})
	if _, err := RunFS(context.Background(), env, raw); err == nil {
		t.Fatal("缺失文件应报 error")
	}
	// 二进制文件：操作性失败（PNG magic 嗅探为 octet-stream 且非合法 UTF-8；
	// 注意 FF FE 前缀会被 Go 嗅探为 utf-16le 文本，不能用来当二进制样本）
	runBin := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00}
	if err := env.FS.WriteFile("/b.bin", runBin, 0o644); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(map[string]any{"action": "edit", "path": "/b.bin",
		"edits": []map[string]string{{"oldText": "a", "newText": "b"}}})
	if _, err := RunFS(context.Background(), env, raw); err == nil ||
		!strings.Contains(err.Error(), "not a text file") {
		t.Fatalf("二进制应报 not a text file: %v", err)
	}
}

func TestContentVersionStable(t *testing.T) {
	if contentVersion("abc") != contentVersion("abc") {
		t.Fatal("同内容同版本")
	}
	if contentVersion("abc") == contentVersion("abd") {
		t.Fatal("不同内容应不同版本")
	}
	// 写后 v = 内容版本（跨调用一致）
	env := newTestEnv(t)
	w := parseOutcome(t, runWrite(t, env, "/v.txt", "alpha\n"))
	if w["v"] != contentVersion("alpha\n") {
		t.Fatalf("write v 与内容版本不一致: %v", w["v"])
	}
	e := parseOutcome(t, runEdit(t, env, "/v.txt", map[string]string{"oldText": "alpha", "newText": "beta"}))
	if e["v"] != contentVersion("beta\n") {
		t.Fatalf("edit v 与内容版本不一致: %v", e["v"])
	}
}
