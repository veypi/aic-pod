package host

import (
	"strings"
	"testing"
)

// winToOS 是纯函数（跨平台可测）——/c/ 规范形 → Windows OS 路径的单一映射点。
func TestWinToOS(t *testing.T) {
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		{`/c`, `C:\`, false},
		{`/c/Users/x`, `C:\Users\x`, false},
		{`/d/work/a b.txt`, `D:\work\a b.txt`, false},
		// 旧输入形容错归一
		{`/C:/x`, `C:\x`, false},
		{`C:\x`, `C:\x`, false},
		{`c:/x`, `C:\x`, false},
		{`//c/x`, `C:\x`, false},
		// 虚拟根与非法形态
		{`/`, ``, true},
		{`/users`, ``, true}, // 非盘符绝对路径：虚拟根下只有盘符挂载
		{`rel/x`, ``, true},  // 相对路径不得到达 OS 边界
		{`/C:foo`, ``, true}, // 盘符相对形态非盘符根
	}
	for _, c := range cases {
		got, err := winToOS(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("winToOS(%q) = %q, want error", c.in, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("winToOS(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

func TestDriveEntriesCanonicalNames(t *testing.T) {
	entries := driveEntries([]string{"C:", "D:"})
	if len(entries) != 2 || entries[0].Name() != "c" || entries[1].Name() != "d" {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("driveEntries names = %v, want [c d]", names)
	}
	if !entries[0].IsDir() {
		t.Fatal("drive entries must be dirs")
	}
	// 子路径拼接得规范形："/" + "c" = "/c"。
	if got := "/" + entries[0].Name(); got != "/c" {
		t.Fatalf("join = %q", got)
	}
}

// 错误文案引导 /c/ 规范形。
func TestWinToOSErrorGuidesCanonical(t *testing.T) {
	_, err := winToOS("/users")
	if err == nil || !strings.Contains(err.Error(), "/c/") {
		t.Fatalf("error should guide /c/ form: %v", err)
	}
}
