package aichost

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseActionArgvStrict(t *testing.T) {
	flags := flagSet{"content": flagValue, "limit": flagValue, "append": flagBool}

	pa, err := parseActionArgv("fs", "write", []string{"/a.txt", "--content", "x y", "--append"}, flags)
	if err != nil {
		t.Fatal(err)
	}
	if pa.flags["content"] != "x y" || !pa.bools["append"] || len(pa.positional) != 1 {
		t.Fatalf("pa = %+v", pa)
	}

	// bool flag 不吞下一元素
	pa, err = parseActionArgv("fs", "search", []string{"--append", "/app"}, flags)
	if err != nil {
		t.Fatal(err)
	}
	if len(pa.positional) != 1 || pa.positional[0] != "/app" {
		t.Fatalf("positional = %v", pa.positional)
	}

	// -- 终止符
	pa, err = parseActionArgv("fs", "read", []string{"--", "--limit", "5"}, flags)
	if err != nil {
		t.Fatal(err)
	}
	if len(pa.positional) != 2 || pa.positional[0] != "--limit" {
		t.Fatalf("positional = %v", pa.positional)
	}

	// 未知 flag
	if _, err := parseActionArgv("fs", "read", []string{"--lmit", "5"}, flags); err == nil ||
		!strings.Contains(err.Error(), `unknown flag "--lmit"`) {
		t.Fatalf("err = %v", err)
	}
	// --flag=value
	if _, err := parseActionArgv("fs", "read", []string{"--limit=5"}, flags); err == nil ||
		!strings.Contains(err.Error(), `invalid flag "--limit=5"`) {
		t.Fatalf("err = %v", err)
	}
	// requires a value
	if _, err := parseActionArgv("fs", "read", []string{"/a", "--limit"}, flags); err == nil ||
		!strings.Contains(err.Error(), `flag "--limit" requires a value`) {
		t.Fatalf("err = %v", err)
	}
}

func TestLooksLikeFlagNameMustStartWithLetter(t *testing.T) {
	cases := map[string]bool{
		"content": true, "replace-all": true, "a1": true,
		"1abc": false, "-abc": false, "_abc": false, "": false,
	}
	for in, want := range cases {
		if got := looksLikeFlagName(in); got != want {
			t.Errorf("looksLikeFlagName(%q) = %v, want %v", in, got, want)
		}
	}
}

func setupFsDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func TestFsWriteReadAppend(t *testing.T) {
	dir := setupFsDir(t)
	p := filepath.Join(dir, "a.txt")

	r, _ := handleFsWrite(mustArgv(t, "write", []string{p, "--content", "line1\nline2\n"}))
	if r.Error != "" {
		t.Fatal(r.Error)
	}
	if !strings.Contains(r.Content, "wrote file: "+p+" (2 lines, 12 bytes)") {
		t.Fatalf("content = %q", r.Content)
	}
	if r.Attrs["mode"] != "overwrite" {
		t.Fatalf("mode = %q", r.Attrs["mode"])
	}

	r, _ = handleFsWrite(mustArgv(t, "write", []string{p, "--content", "line3\n", "--append"}))
	if r.Error != "" {
		t.Fatal(r.Error)
	}
	if !strings.Contains(r.Content, "appended to "+p+" (+1 lines, +6 bytes)") {
		t.Fatalf("content = %q", r.Content)
	}
	if r.Attrs["mode"] != "append" {
		t.Fatalf("mode = %q", r.Attrs["mode"])
	}

	r, _ = handleFsRead(mustArgv(t, "read", []string{p}))
	if r.Error != "" {
		t.Fatal(r.Error)
	}
	if r.Content != "1\tline1\n2\tline2\n3\tline3\n" {
		t.Fatalf("content = %q", r.Content)
	}
	if r.Attrs["total_lines"] != "3" || r.Attrs["rows"] != "3" || r.Attrs["range"] != "1-3" || r.Attrs["truncated"] != "false" {
		t.Fatalf("attrs = %v", r.Attrs)
	}

	// --content 必填
	r, _ = handleFsWrite(mustArgv(t, "write", []string{p, "fallback"}))
	if r.Error != "fs write: --content is required" {
		t.Fatalf("error = %q", r.Error)
	}
}

func TestFsReadOffsetLimit(t *testing.T) {
	dir := setupFsDir(t)
	p := filepath.Join(dir, "f.txt")
	os.WriteFile(p, []byte("a\nb\nc\nd\ne\n"), 0644)

	r, _ := handleFsRead(mustArgv(t, "read", []string{p, "--offset", "2", "--limit", "2"}))
	if r.Error != "" {
		t.Fatal(r.Error)
	}
	if r.Content != "2\tb\n3\tc\n" {
		t.Fatalf("content = %q", r.Content)
	}
	if r.Attrs["range"] != "2-3" || r.Attrs["truncated"] != "true" || r.Attrs["total_lines"] != "5" || r.Attrs["rows"] != "2" {
		t.Fatalf("attrs = %v", r.Attrs)
	}

	r, _ = handleFsRead(mustArgv(t, "read", []string{p, "--offset", "100"}))
	if r.Error != "fs read: offset 100 exceeds 5 lines" {
		t.Fatalf("error = %q", r.Error)
	}
	r, _ = handleFsRead(mustArgv(t, "read", []string{p, "--offset", "0"}))
	if r.Error != "fs read: offset must be >= 1, got 0" {
		t.Fatalf("error = %q", r.Error)
	}
}

func TestFsEditMultiMatch(t *testing.T) {
	dir := setupFsDir(t)
	p := filepath.Join(dir, "e.txt")
	os.WriteFile(p, []byte("foo x foo\n"), 0644)

	r, _ := handleFsEdit(mustArgv(t, "edit", []string{p, "--old", "foo", "--new", "bar"}))
	if !strings.Contains(r.Error, "--old matches 2 locations") {
		t.Fatalf("error = %q", r.Error)
	}
	r, _ = handleFsEdit(mustArgv(t, "edit", []string{p, "--old", "foo", "--new", "bar", "--replace-all"}))
	if r.Error != "" {
		t.Fatal(r.Error)
	}
	data, _ := os.ReadFile(p)
	if string(data) != "bar x bar\n" {
		t.Fatalf("data = %q", data)
	}
}

func TestFsRmRootProtection(t *testing.T) {
	root := "/"
	if strings.Contains(filepath.VolumeName(os.TempDir()), ":") {
		root = filepath.VolumeName(os.TempDir()) + `\`
	}
	r, _ := handleFsRemove(mustArgv(t, "rm", []string{root}))
	if r.State != "rejected" || !strings.Contains(r.Error, "cannot remove root directory") {
		t.Fatalf("result = %+v", r)
	}
}

func TestFsLsByteOrder(t *testing.T) {
	dir := setupFsDir(t)
	for _, name := range []string{"b.txt", "Z.txt", "a.txt"} {
		os.WriteFile(filepath.Join(dir, name), []byte("x"), 0644)
	}
	os.MkdirAll(filepath.Join(dir, "docs"), 0755)
	r, _ := handleFsLs(mustArgv(t, "ls", []string{dir}))
	if r.Error != "" {
		t.Fatal(r.Error)
	}
	want := "1\tZ.txt\n2\ta.txt\n3\tb.txt\n4\tdocs/"
	if r.Content != want {
		t.Fatalf("content = %q, want %q", r.Content, want)
	}
}

func TestFsSearch(t *testing.T) {
	dir := setupFsDir(t)
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "main.go"), []byte("package main\n// TODO fix\ndone\n"), 0644)
	os.WriteFile(filepath.Join(dir, "notes.md"), []byte("# notes\n"), 0644)

	// glob 模式
	r, _ := handleFsSearch(mustArgv(t, "search", []string{dir, "--glob", "*.go"}))
	if r.Error != "" {
		t.Fatal(r.Error)
	}
	if !strings.Contains(r.Content, "main.go\t") || r.Attrs["rows"] != "1" || r.Attrs["truncated"] != "false" {
		t.Fatalf("content = %q attrs = %v", r.Content, r.Attrs)
	}

	// grep 模式：全行 glob，无列号
	r, _ = handleFsSearch(mustArgv(t, "search", []string{dir, "--pattern", "*TODO*"}))
	if r.Error != "" {
		t.Fatal(r.Error)
	}
	if !strings.Contains(r.Content, ":2\t// TODO fix") {
		t.Fatalf("content = %q", r.Content)
	}

	// 无通配符 pattern 的空结果带引导提示
	r, _ = handleFsSearch(mustArgv(t, "search", []string{dir, "--pattern", "TODO"}))
	if !strings.Contains(r.Content, `(pattern is full-line glob; use "*text*" for substring match)`) {
		t.Fatalf("content = %q", r.Content)
	}

	// limit+1：恰好等于 limit 不误标
	os.WriteFile(filepath.Join(dir, "f1.txt"), []byte("x"), 0644)
	os.WriteFile(filepath.Join(dir, "f2.txt"), []byte("x"), 0644)
	os.WriteFile(filepath.Join(dir, "f3.txt"), []byte("x"), 0644)
	r, _ = handleFsSearch(mustArgv(t, "search", []string{dir, "--glob", "f?.txt", "--limit", "3"}))
	if r.Attrs["truncated"] != "false" || r.Attrs["rows"] != "3" {
		t.Fatalf("attrs = %v", r.Attrs)
	}
	r, _ = handleFsSearch(mustArgv(t, "search", []string{dir, "--glob", "f?.txt", "--limit", "2"}))
	if r.Attrs["truncated"] != "true" || r.Attrs["rows"] != "2" {
		t.Fatalf("attrs = %v", r.Attrs)
	}
}

func TestFsDownloadSchemeValidation(t *testing.T) {
	dir := setupFsDir(t)
	dst := filepath.Join(dir, "x")
	r, _ := handleFsDownload(mustArgv(t, "download", []string{dst, "--from", "C:/Users/a"}))
	if !strings.Contains(r.Error, "missing scheme") {
		t.Fatalf("error = %q", r.Error)
	}
	r, _ = handleFsDownload(mustArgv(t, "download", []string{dst, "--from", "cloud://$USER/a.txt"}))
	if r.Error != "fs download: scheme not yet supported: cloud" {
		t.Fatalf("error = %q", r.Error)
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"*TODO*", "fix TODO now", true},
		{"TODO", "TODO", true},
		{"TODO", "xTODO", false},
		{"log*Error", "log something Error", true},
		{"?.txt", "a.txt", true},
		{"?.txt", "ab.txt", false},
		{"*.md", "notes.md", true},
		{"*", "anything", true},
	}
	for _, c := range cases {
		if got := globMatch(c.pattern, c.s); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}

func TestFsUnknownAndMissingAction(t *testing.T) {
	r, _ := handleFs(context.Background(), &ToolParams{Argv: []string{"/tmp"}})
	if !strings.HasPrefix(r.Error, "fs: action is required (supported:") {
		t.Fatalf("error = %q", r.Error)
	}
	r, _ = handleFs(context.Background(), &ToolParams{Action: "nope", Argv: []string{}})
	if !strings.Contains(r.Error, `fs: unknown action "nope"`) {
		t.Fatalf("error = %q", r.Error)
	}
}

func mustArgv(t *testing.T, action string, argv []string) *parsedArgv {
	t.Helper()
	pa, err := parseActionArgv("fs", action, argv, fsFlagSets[action])
	if err != nil {
		t.Fatal(err)
	}
	return pa
}
