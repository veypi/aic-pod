package proto

import "testing"

// ResolvePath 固定向量（§2.1.1 可解析层）：纯路径运算，双端结果必须一致。

var vecVars = map[string]string{
	"$USER":    "/users/u1",
	"$AGENT":   "/agents/a1",
	"$SESSION": "/sessions/s1",
}

func TestResolvePathVectors(t *testing.T) {
	cases := []struct {
		path, workdir string
		vars          map[string]string
		want          string
		wantErr       bool
	}{
		// 根变量展开（忽略 workdir）
		{"$USER/a.txt", "/whatever", vecVars, "/users/u1/a.txt", false},
		{"$USER", "", vecVars, "/users/u1", false},
		{"$SESSION", "", vecVars, "/sessions/s1", false},
		{"$AGENT/../x", "", vecVars, "", true}, // 逃逸变量根
		{"$USER/sub/../../x", "", vecVars, "", true},
		{"$USER/./a//b", "", vecVars, "/users/u1/a/b", false},
		// 变量名后必须跟 / 或结束：$USERX 不匹配 $USER，按字面相对路径
		{"$USERX/a", "/wd", vecVars, "/wd/$USERX/a", false},
		// 绝对路径忽略 workdir
		{"/abs/path", "/wd", nil, "/abs/path", false},
		{"/abs/../clean", "", nil, "/clean", false},
		// 相对路径基于 workdir
		{"rel/file", "/wd", nil, "/wd/rel/file", false},
		{".", "/wd", nil, "/wd", false},
		{"./sub", "/wd", nil, "/wd/sub", false},
		{"../up", "/wd/sub", nil, "/wd/up", false},
		// workdir 约束
		{"rel", "", nil, "", true},
		{"rel", "not/abs", nil, "", true},
		// Windows 盘符视为绝对
		{`C:\foo\bar`, "/wd", nil, `C:\foo\bar`, false},
		{"", "/wd", nil, "", true},
	}
	for _, c := range cases {
		got, err := ResolvePath(c.path, c.workdir, c.vars)
		if c.wantErr {
			if err == nil {
				t.Errorf("ResolvePath(%q, %q) = %q, want error", c.path, c.workdir, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("ResolvePath(%q, %q) = %q, %v; want %q", c.path, c.workdir, got, err, c.want)
		}
	}
}

func TestWithinRoots(t *testing.T) {
	roots := []string{"/users/u1", "/agents/a1", "/sessions/s1"}
	in := []string{"/users/u1", "/users/u1/a/b", "/sessions/s1/x.txt", "/agents/a1"}
	out := []string{"/users/u2", "/users/u1x", "/users", "/", "/sessions/s1/../s2", ""}
	for _, p := range in {
		if !WithinRoots(p, roots) {
			t.Errorf("WithinRoots(%q) = false, want true", p)
		}
	}
	for _, p := range out {
		if WithinRoots(p, roots) {
			t.Errorf("WithinRoots(%q) = true, want false", p)
		}
	}
}
