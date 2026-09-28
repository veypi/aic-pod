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
		// Windows 盘符输入归一为 /c/… 类 Linux 规范形（2026-09-24 全局统一，
		// 废除 C:/ 规范形）：反斜杠归一为斜杠、盘符字母小写作首段；
		// 裸盘符 = 盘符根（/c）
		{`C:\foo\bar`, "/wd", nil, `/c/foo/bar`, false},
		{`C:\foo\..\x`, "/wd", nil, `/c/x`, false},
		{`C:\`, "/wd", nil, `/c`, false},
		{"C:/", "/wd", nil, "/c", false},
		{"C:/slash/form", "/wd", nil, "/c/slash/form", false},
		// 裸盘符 = 盘符根；盘符相对形态 C:foo 保持相对路径语义
		{"C:", "/wd", nil, "/c", false},
		{"C:foo", "/wd", nil, "/wd/C:foo", false},
		// 盘符字母小写归一（根保护等值比较依赖单一规范形）
		{"c:/x", "/wd", nil, "/c/x", false},
		{"c:", "/wd", nil, "/c", false},
		{`c:\x`, "/wd", nil, `/c/x`, false},
		// 前导斜杠+盘符形归一（host 端输入容错收口）
		{"/C:", "/wd", nil, "/c", false},
		{"/C:/x", "/wd", nil, "/c/x", false},
		{`/C:\x`, "/wd", nil, `/c/x`, false},
		{"/c:/x", "/wd", nil, "/c/x", false},
		// 多斜杠前缀先 path.Clean 折叠再判盘符形——归一结果中 //c、/C: 形态不存在
		//（根保护等值比较依赖单一规范形；修复前 //C: → "/C:" 绕过 rm/mv 根保护）
		{"//C:", "/wd", nil, "/c", false},
		{"//C:/x", "/wd", nil, "/c/x", false},
		{"///C:/x", "/wd", nil, "/c/x", false},
		{"//c:", "/wd", nil, "/c", false},
		{"C://x", "/wd", nil, "/c/x", false},
		{"/C://x", "/wd", nil, "/c/x", false},
		// /C:foo 不是盘符形（C:foo 为盘符相对），保持 POSIX 绝对路径
		{"/C:foo", "/wd", nil, "/C:foo", false},
		// workdir 盘符形同样归一
		{"rel.txt", `C:\wd`, nil, "/c/wd/rel.txt", false},
		{"rel.txt", "c:", nil, "/c/rel.txt", false},
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

// WinTmpToOS 固定向量（纯函数，跨平台可测）：/tmp 虚拟别名 → windows 临时
// 目录原生路径；非 /tmp 前缀一律 ok=false（/tmpfoo 不是别名）。
func TestWinTmpToOS(t *testing.T) {
	tmp := `C:\Users\x\AppData\Local\Temp`
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"/tmp", tmp, true},
		{"/tmp/", tmp, true},
		{"/tmp/a", tmp + `\a`, true},
		{"/tmp/a b/c.txt", tmp + `\a b\c.txt`, true},
		{"/tmpfoo", "", false},
		{"/c/tmp", "", false},
		{"/", "", false},
		{"tmp", "", false},
	}
	for _, c := range cases {
		got, ok := WinTmpToOS(c.in, tmp)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("WinTmpToOS(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
	// 尾反斜杠注入防御（os.TempDir 一般不帶，注入侧容错）
	if got, ok := WinTmpToOS("/tmp/a", tmp+`\`); !ok || got != tmp+`\a` {
		t.Errorf("WinTmpToOS trailing-backslash = %q, %v", got, ok)
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
