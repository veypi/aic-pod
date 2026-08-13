package vcore

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// runJSON 执行 json 虚拟指令（MemVFS + 根目录 /），返回 (Result, error)。
func runJSON(t *testing.T, vfs *MemVFS, argv ...string) (*Result, error) {
	t.Helper()
	return Run(context.Background(), &Env{VFS: vfs, Workdir: "/"}, "json", argv)
}

// runJSONNoErr 执行 json 虚拟指令并断言无错（返回 Result）。
func runJSONNoErr(t *testing.T, vfs *MemVFS, argv ...string) *Result {
	t.Helper()
	res, err := Run(context.Background(), &Env{VFS: vfs, Workdir: "/"}, "json", argv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return res
}

const testDoc = `{
  "name": "demo",
  "count": 42,
  "ok": true,
  "tags": ["a", "b", "c"],
  "nested": {
    "inner": {
      "x": 1,
      "y": "text"
    },
    "items": [10, 20]
  },
  "nothing": null
}`

func newTestDoc(t *testing.T) *MemVFS {
	t.Helper()
	vfs := NewMemVFS()
	vfs.SetFile("/doc.json", []byte(testDoc), testTime)
	return vfs
}

func TestJSONViewSkeleton(t *testing.T) {
	vfs := newTestDoc(t)
	res := runJSONNoErr(t, vfs, "view", "doc.json")
	want := "/doc.json: object (6 keys)\n" +
		"  count: number\n" +
		"  name: string(4)\n" +
		"  nested: object (2 keys)\n" +
		"    inner: object (2 keys)\n" +
		"      x: number\n" +
		"      y: string(4)\n" +
		"    items: array[2]\n" +
		"  nothing: null\n" +
		"  ok: bool\n" +
		"  tags: array[3]"
	if res.Content != want {
		t.Errorf("skeleton mismatch:\n got: %s\nwant: %s", res.Content, want)
	}
}

func TestJSONViewDepth(t *testing.T) {
	vfs := newTestDoc(t)
	res := runJSONNoErr(t, vfs, "view", "doc.json", "--depth", "1")
	want := "/doc.json: object (6 keys)\n" +
		"  count: number\n" +
		"  name: string(4)\n" +
		"  nested: object (2 keys)\n" +
		"  nothing: null\n" +
		"  ok: bool\n" +
		"  tags: array[3]"
	if res.Content != want {
		t.Errorf("depth=1 mismatch:\n got: %s\nwant: %s", res.Content, want)
	}
}

func TestJSONViewDepth2(t *testing.T) {
	vfs := newTestDoc(t)
	res := runJSONNoErr(t, vfs, "view", "doc.json", "--depth", "2")
	want := "/doc.json: object (6 keys)\n" +
		"  count: number\n" +
		"  name: string(4)\n" +
		"  nested: object (2 keys)\n" +
		"    inner: object (2 keys)\n" +
		"    items: array[2]\n" +
		"  nothing: null\n" +
		"  ok: bool\n" +
		"  tags: array[3]"
	if res.Content != want {
		t.Errorf("depth=2 mismatch:\n got: %s\nwant: %s", res.Content, want)
	}
}

func TestJSONViewValues(t *testing.T) {
	vfs := newTestDoc(t)
	res := runJSONNoErr(t, vfs, "view", "doc.json", "--values", "--depth", "1")
	want := "/doc.json: object (6 keys)\n" +
		"  count: number 42\n" +
		"  name: string(4) \"demo\"\n" +
		"  nested: object (2 keys)\n" +
		"  nothing: null\n" +
		"  ok: bool true\n" +
		"  tags: array[3]"
	if res.Content != want {
		t.Errorf("values mismatch:\n got: %s\nwant: %s", res.Content, want)
	}
}

func TestJSONViewKey(t *testing.T) {
	vfs := newTestDoc(t)
	res := runJSONNoErr(t, vfs, "view", "doc.json", "--key", "nested.inner")
	want := "{\n  \"x\": 1,\n  \"y\": \"text\"\n}"
	if res.Content != want {
		t.Errorf("key mismatch:\n got: %s\nwant: %s", res.Content, want)
	}
}

func TestJSONViewKeyCompact(t *testing.T) {
	vfs := newTestDoc(t)
	res := runJSONNoErr(t, vfs, "view", "doc.json", "--key", "tags[1]", "--compact")
	if res.Content != `"b"` {
		t.Errorf("key compact mismatch: %s", res.Content)
	}
}

func TestJSONViewKeyDotIndex(t *testing.T) {
	vfs := newTestDoc(t)
	res := runJSONNoErr(t, vfs, "view", "doc.json", "--key", "nested.items.1", "--compact")
	if res.Content != "20" {
		t.Errorf("dot index mismatch: %s", res.Content)
	}
}

func TestJSONViewRaw(t *testing.T) {
	vfs := newTestDoc(t)
	res := runJSONNoErr(t, vfs, "view", "doc.json", "--raw")
	if res.Content != testDoc {
		t.Errorf("raw mismatch:\n got: %s\nwant: %s", res.Content, testDoc)
	}
}

func TestJSONViewMissingKey(t *testing.T) {
	vfs := newTestDoc(t)
	_, err := runJSON(t, vfs, "view", "doc.json", "--key", "nope")
	if err == nil || !strings.Contains(err.Error(), "key \"nope\" not found") {
		t.Errorf("want missing key error, got %v", err)
	}
}

func TestJSONViewNotJSON(t *testing.T) {
	vfs := NewMemVFS()
	vfs.SetFile("/bad.json", []byte("not json"), testTime)
	_, err := runJSON(t, vfs, "view", "bad.json")
	if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("want not valid JSON error, got %v", err)
	}
}

func TestJSONViewRawConflict(t *testing.T) {
	vfs := newTestDoc(t)
	_, err := runJSON(t, vfs, "view", "doc.json", "--raw", "--key", "a")
	if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Errorf("want conflict error, got %v", err)
	}
}

// ---- set ----

func TestJSONSetString(t *testing.T) {
	vfs := newTestDoc(t)
	runJSONNoErr(t, vfs, "set", "doc.json", "name", "hello world")
	res := runJSONNoErr(t, vfs, "view", "doc.json", "--key", "name", "--compact")
	if res.Content != `"hello world"` {
		t.Errorf("set string mismatch: %s", res.Content)
	}
}

func TestJSONSetNumber(t *testing.T) {
	vfs := newTestDoc(t)
	runJSONNoErr(t, vfs, "set", "doc.json", "count", "123")
	res := runJSONNoErr(t, vfs, "view", "doc.json", "--key", "count", "--compact")
	if res.Content != "123" {
		t.Errorf("set number mismatch: %s", res.Content)
	}
}

func TestJSONSetObject(t *testing.T) {
	vfs := newTestDoc(t)
	runJSONNoErr(t, vfs, "set", "doc.json", "nested.inner", `{"x": 9}`)
	res := runJSONNoErr(t, vfs, "view", "doc.json", "--key", "nested.inner", "--compact")
	if res.Content != `{"x":9}` {
		t.Errorf("set object mismatch: %s", res.Content)
	}
}

func TestJSONSetAutoCreate(t *testing.T) {
	vfs := newTestDoc(t)
	runJSONNoErr(t, vfs, "set", "doc.json", "a.b.c", "1")
	res := runJSONNoErr(t, vfs, "view", "doc.json", "--key", "a.b.c", "--compact")
	if res.Content != "1" {
		t.Errorf("auto-create mismatch: %s", res.Content)
	}
}

func TestJSONSetArrayIndex(t *testing.T) {
	vfs := newTestDoc(t)
	runJSONNoErr(t, vfs, "set", "doc.json", "tags[0]", "z")
	res := runJSONNoErr(t, vfs, "view", "doc.json", "--key", "tags[0]", "--compact")
	if res.Content != `"z"` {
		t.Errorf("array set mismatch: %s", res.Content)
	}
}

func TestJSONSetOutOfRange(t *testing.T) {
	vfs := newTestDoc(t)
	_, err := runJSON(t, vfs, "set", "doc.json", "tags[9]", "z")
	if err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Errorf("want out of range error, got %v", err)
	}
}

// ---- del ----

func TestJSONDelKey(t *testing.T) {
	vfs := newTestDoc(t)
	runJSONNoErr(t, vfs, "del", "doc.json", "nothing")
	_, err := runJSON(t, vfs, "view", "doc.json", "--key", "nothing")
	if err == nil {
		t.Error("key nothing should be gone")
	}
}

func TestJSONDelArrayElement(t *testing.T) {
	vfs := newTestDoc(t)
	runJSONNoErr(t, vfs, "del", "doc.json", "tags[1]")
	res := runJSONNoErr(t, vfs, "view", "doc.json", "--key", "tags", "--compact")
	if res.Content != `["a","c"]` {
		t.Errorf("array del mismatch: %s", res.Content)
	}
}

func TestJSONDelMissing(t *testing.T) {
	vfs := newTestDoc(t)
	_, err := runJSON(t, vfs, "del", "doc.json", "nope")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("want not found error, got %v", err)
	}
}

// ---- append ----

func TestJSONAppend(t *testing.T) {
	vfs := newTestDoc(t)
	runJSONNoErr(t, vfs, "append", "doc.json", "tags", "d")
	res := runJSONNoErr(t, vfs, "view", "doc.json", "--key", "tags", "--compact")
	if res.Content != `["a","b","c","d"]` {
		t.Errorf("append mismatch: %s", res.Content)
	}
}

func TestJSONAppendCreate(t *testing.T) {
	vfs := newTestDoc(t)
	runJSONNoErr(t, vfs, "append", "doc.json", "newlist", "1")
	res := runJSONNoErr(t, vfs, "view", "doc.json", "--key", "newlist", "--compact")
	if res.Content != `[1]` {
		t.Errorf("append create mismatch: %s", res.Content)
	}
}

func TestJSONAppendNestedCreate(t *testing.T) {
	vfs := newTestDoc(t)
	runJSONNoErr(t, vfs, "append", "doc.json", "x.y", "1")
	res := runJSONNoErr(t, vfs, "view", "doc.json", "--key", "x.y", "--compact")
	if res.Content != `[1]` {
		t.Errorf("nested append create mismatch: %s", res.Content)
	}
}

func TestJSONAppendNotArray(t *testing.T) {
	vfs := newTestDoc(t)
	_, err := runJSON(t, vfs, "append", "doc.json", "count", "1")
	if err == nil || !strings.Contains(err.Error(), "not an array") {
		t.Errorf("want not an array error, got %v", err)
	}
}

// ---- merge ----

func TestJSONMerge(t *testing.T) {
	vfs := newTestDoc(t)
	runJSONNoErr(t, vfs, "merge", "doc.json", `{"count": 1, "added": true}`)
	res := runJSONNoErr(t, vfs, "view", "doc.json", "--key", "count", "--compact")
	if res.Content != "1" {
		t.Errorf("merge overwrite mismatch: %s", res.Content)
	}
	res = runJSONNoErr(t, vfs, "view", "doc.json", "--key", "added", "--compact")
	if res.Content != "true" {
		t.Errorf("merge add mismatch: %s", res.Content)
	}
	// 浅合并：nested 整体替换
	runJSONNoErr(t, vfs, "merge", "doc.json", `{"nested": {"inner": {"x": 99}}}`)
	res = runJSONNoErr(t, vfs, "view", "doc.json", "--key", "nested", "--compact")
	if res.Content != `{"inner":{"x":99}}` {
		t.Errorf("merge shallow mismatch: %s", res.Content)
	}
}

func TestJSONMergeNonObject(t *testing.T) {
	vfs := newTestDoc(t)
	_, err := runJSON(t, vfs, "merge", "doc.json", `[1,2]`)
	if err == nil || !strings.Contains(err.Error(), "must be a JSON object") {
		t.Errorf("want non-object merge error, got %v", err)
	}
}

// ---- 输出与错误 ----

func TestJSONSetOKOutput(t *testing.T) {
	vfs := newTestDoc(t)
	res := runJSONNoErr(t, vfs, "set", "doc.json", "name", "x")
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatalf("OK output not JSON: %v", err)
	}
	if out["ok"] != true || out["path"] != "doc.json" {
		t.Errorf("OK output mismatch: %s", res.Content)
	}
}

func TestJSONHelp(t *testing.T) {
	vfs := newTestDoc(t)
	res := runJSONNoErr(t, vfs, "--help")
	if !strings.Contains(res.Content, "view") || !strings.Contains(res.Content, "merge") {
		t.Errorf("help missing subcommands: %s", res.Content)
	}
	res = runJSONNoErr(t, vfs, "view", "--help")
	if !strings.Contains(res.Content, "--key") || !strings.Contains(res.Content, "--depth") {
		t.Errorf("sub help missing flags: %s", res.Content)
	}
}

func TestJSONUnknownSub(t *testing.T) {
	vfs := newTestDoc(t)
	_, err := runJSON(t, vfs, "wat")
	if err == nil || !strings.Contains(err.Error(), "unknown subcommand") {
		t.Errorf("want unknown subcommand error, got %v", err)
	}
}

func TestJSONLevels(t *testing.T) {
	if got := ExecRequired("json", []string{"view", "x.json"}); got != 1 {
		t.Errorf("json view level = %d, want 1", got)
	}
	for _, sub := range []string{"set", "del", "append", "merge"} {
		if got := ExecRequired("json", []string{sub, "x.json", "k"}); got != 2 {
			t.Errorf("json %s level = %d, want 2", sub, got)
		}
	}
	if got := ExecRequired("json", []string{"unknown"}); got != 2 {
		t.Errorf("json unknown level = %d, want 2", got)
	}
}
