package fsx

import "testing"

// TestEnvResolveWindowsDrivePath Windows 盘符形输入是绝对路径，必须收口为规范形
// /c/…，不能拼到 Workdir 下（2026-10-07 win 实测：fs ls C:/Users/v 被拼成
// "…/ivec/C:/Users/v"，切段后留下 "C:" 段 → invalid_argument: Invalid path segment）。
func TestEnvResolveWindowsDrivePath(t *testing.T) {
	env := &Env{Workdir: "C:/Users/v/ivec"}
	for _, c := range []struct{ in, want string }{
		{"C:/Users/v/notes.txt", "/c/Users/v/notes.txt"},
		{"c:/Users/v", "/c/Users/v"},
		{`C:\Users\v`, "/c/Users/v"},
		{"C:", "/c"},
		{"/C:/Users/v", "/c/Users/v"},
		{"/c/Users/v", "/c/Users/v"},
		// 相对名照旧拼 Workdir（Workdir 自身是盘符形，由 hostfs 在切段前收口）。
		{"notes.txt", "C:/Users/v/ivec/notes.txt"},
	} {
		got, err := env.resolve(c.in, true)
		if err != nil {
			t.Fatalf("resolve(%q, windows) err = %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("resolve(%q, windows) = %q; want %q", c.in, got, c.want)
		}
	}
	// 非 Windows 主机：盘符形不是绝对路径（保持原语义，拼 Workdir）。
	if got, err := env.resolve("C:/x", false); err != nil || got != "C:/Users/v/ivec/C:/x" {
		t.Errorf("resolve(C:/x, posix) = %q, %v; want Workdir 下拼接", got, err)
	}
}

// TestEnvResolveRelativeAndAbsolute 常规 POSIX 语义不受影响。
func TestEnvResolveRelativeAndAbsolute(t *testing.T) {
	env := &Env{Workdir: "/u/admin"}
	for _, c := range []struct{ in, want string }{
		{"notes.txt", "/u/admin/notes.txt"},
		{"/u/admin/notes.txt", "/u/admin/notes.txt"},
		{"/u/admin/../other", "/u/other"},
		{"./a/", "/u/admin/a"},
	} {
		got, err := env.resolve(c.in, false)
		if err != nil {
			t.Fatalf("resolve(%q) err = %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("resolve(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}
