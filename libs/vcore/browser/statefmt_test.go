package browser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeJSON(t *testing.T, path string, doc any) {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readDoc(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func cookie(domain, path, name, value string, expires float64) map[string]any {
	return map[string]any{"domain": domain, "path": path, "name": name, "value": value,
		"expires": expires, "httpOnly": false, "secure": true, "sameSite": "Lax"}
}

// TestMergeStateSiteIsolation：跨 session 站点不互覆（todo §6 验收向量）——
// session A 保存（站点 a.com）后 session B 保存（站点 b.com）：a.com 保留。
func TestMergeStateSiteIsolation(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "browser.json")
	now := time.Now()

	writeJSON(t, dst, map[string]any{
		"cookies": []any{cookie("a.com", "/", "sid", "A", float64(now.Add(time.Hour).Unix()))},
		"origins": []any{map[string]any{"origin": "https://a.com",
			"localStorage":   []any{map[string]any{"name": "k1", "value": "va"}},
			"sessionStorage": []any{}}},
	})
	sum := mergeStateFile(dst, writeSrc(t, map[string]any{
		"cookies": []any{cookie("b.com", "/", "sid", "B", float64(now.Add(time.Hour).Unix()))},
		"origins": []any{map[string]any{"origin": "https://b.com",
			"localStorage": []any{map[string]any{"name": "k2", "value": "vb"}}}},
	}), now)
	if sum == nil {
		t.Fatal("mergeStateFile returned nil")
	}
	if sum.Sites != 2 || sum.Updated == 0 || sum.Kept == 0 {
		t.Errorf("summary = %+v, want sites=2 updated>0 kept>0", sum)
	}
	out := readDoc(t, dst)
	cookies := out["cookies"].([]any)
	if len(cookies) != 2 {
		t.Fatalf("cookies = %d, want 2 (a.com kept + b.com new)", len(cookies))
	}
	origins := out["origins"].([]any)
	if len(origins) != 2 {
		t.Fatalf("origins = %d, want 2", len(origins))
	}
}

// TestMergeStateOverride：同站点同键新覆盖旧；同站点不同键并存。
func TestMergeStateOverride(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "browser.json")
	now := time.Now()
	future := float64(now.Add(time.Hour).Unix())

	writeJSON(t, dst, map[string]any{
		"cookies": []any{
			cookie("a.com", "/", "sid", "OLD", future),
			cookie("a.com", "/", "other", "KEEP", future),
		},
	})
	mergeStateFile(dst, writeSrc(t, map[string]any{
		"cookies": []any{cookie("a.com", "/", "sid", "NEW", future)},
	}), now)

	out := readDoc(t, dst)
	cookies := out["cookies"].([]any)
	if len(cookies) != 2 {
		t.Fatalf("cookies = %d, want 2", len(cookies))
	}
	byName := map[string]map[string]any{}
	for _, c := range cookies {
		m := c.(map[string]any)
		byName[m["name"].(string)] = m
	}
	if byName["sid"]["value"] != "NEW" {
		t.Errorf("sid = %v, want NEW (new wins)", byName["sid"]["value"])
	}
	if byName["other"]["value"] != "KEEP" {
		t.Errorf("other = %v, want KEEP (untouched old kept)", byName["other"]["value"])
	}
}

// TestMergeStateExpiry：过期 cookie 丢弃（两侧都清）；session cookie（expires<=0）保留。
func TestMergeStateExpiry(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "browser.json")
	now := time.Now()
	past := float64(now.Add(-time.Hour).Unix())
	future := float64(now.Add(time.Hour).Unix())

	writeJSON(t, dst, map[string]any{
		"cookies": []any{
			cookie("a.com", "/", "expired-old", "x", past),
			cookie("a.com", "/", "sess", "s", -1),
		},
	})
	sum := mergeStateFile(dst, writeSrc(t, map[string]any{
		"cookies": []any{
			cookie("a.com", "/", "expired-new", "y", past),
			cookie("a.com", "/", "fresh", "z", future),
		},
	}), now)
	if sum == nil {
		t.Fatal("nil summary")
	}
	out := readDoc(t, dst)
	cookies := out["cookies"].([]any)
	if len(cookies) != 2 {
		t.Fatalf("cookies = %d, want 2 (expired dropped both sides, session kept, fresh added)", len(cookies))
	}
	names := map[string]bool{}
	for _, c := range cookies {
		names[c.(map[string]any)["name"].(string)] = true
	}
	if names["expired-old"] || names["expired-new"] {
		t.Errorf("expired cookies must be dropped: %v", names)
	}
	if !names["sess"] || !names["fresh"] {
		t.Errorf("sess + fresh must survive: %v", names)
	}
}

// TestMergeStateFirstSave：无既有档案时直接落新档（仍清过期）。
func TestMergeStateFirstSave(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "browser.json")
	now := time.Now()
	mergeStateFile(dst, writeSrc(t, map[string]any{
		"cookies": []any{
			cookie("a.com", "/", "ok", "1", float64(now.Add(time.Hour).Unix())),
			cookie("a.com", "/", "dead", "2", float64(now.Add(-time.Hour).Unix())),
		},
	}), now)
	out := readDoc(t, dst)
	if n := len(out["cookies"].([]any)); n != 1 {
		t.Fatalf("first save cookies = %d, want 1 (expired cleaned)", n)
	}
}

// TestMergeStateCorruptSrc：新导出损坏 → 不写回、返回 nil（best-effort 不阻断浏览）。
func TestMergeStateCorruptSrc(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "browser.json")
	src := filepath.Join(dir, "src.json")
	writeJSON(t, dst, map[string]any{"cookies": []any{cookie("a.com", "/", "keep", "1", 0)}})
	if err := os.WriteFile(src, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if sum := mergeStateFile(dst, src, time.Now()); sum != nil {
		t.Errorf("corrupt src should yield nil summary, got %+v", sum)
	}
	out := readDoc(t, dst)
	if n := len(out["cookies"].([]any)); n != 1 {
		t.Fatalf("dst must be untouched: cookies = %d, want 1", n)
	}
}

// writeSrc 把新导出写进临时文件并返回其路径（mergeStateFile 的 src 参数）。
func writeSrc(t *testing.T, doc any) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "src.json")
	writeJSON(t, p, doc)
	return p
}
