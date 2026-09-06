package host

import (
	"testing"
)

func TestParseSCPArgv(t *testing.T) {
	// 合法形态
	a, err := parseSCPArgv([]string{"-r", "-P", "2222", "file.txt", "pi:/tmp/"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !a.recursive || a.port != 2222 || a.src != "file.txt" || a.dst != "pi:/tmp/" {
		t.Fatalf("parsed = %+v", a)
	}
	// -P 粘连形态 + -p -q
	a, err = parseSCPArgv([]string{"-P2202", "-p", "-q", "root@host:/a", "/tmp/b"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.port != 2202 || !a.preserve || !a.quiet || a.src != "root@host:/a" || a.dst != "/tmp/b" {
		t.Fatalf("parsed = %+v", a)
	}
	// 无 flag 纯操作数
	if _, err = parseSCPArgv([]string{"/a", "host:/b"}); err != nil {
		t.Fatalf("parse: %v", err)
	}

	bad := [][]string{
		{},                                    // 空
		{"file"},                              // 缺操作数
		{"a", "b", "c"},                       // 多操作数
		{"-o", "ProxyCommand=x", "a", "h:/b"}, // 危险 flag
		{"-F", "cfg", "a", "h:/b"},            // 换 config
		{"-i", "/k", "a", "h:/b"},             // 换密钥
		{"-J", "jump", "a", "h:/b"},           // 跳板
		{"-P"},                                // -P 缺值
		{"-P", "abc", "a", "h:/b"},            // 非法端口
		{"-P", "0", "a", "h:/b"},              // 端口越界
		{"-x", "a", "h:/b"},                   // 未白名单 flag
	}
	for _, argv := range bad {
		if _, err := parseSCPArgv(argv); err == nil {
			t.Errorf("parseSCPArgv(%v) should fail", argv)
		}
	}
}

func TestSplitSCPOperand(t *testing.T) {
	cases := []struct {
		in     string
		remote bool
		head   string
	}{
		{"pi:/tmp/x", true, "pi"},
		{"root@example.com:/a", true, "root@example.com"},
		{"host:", true, "host"}, // 空路径 = 远端 home
		{"example.com:a/b", true, "example.com"},
		{"/tmp/x", false, ""},
		{"./rel", false, ""},
		{"./x:y", false, ""},        // 冒号在斜杠后 = 本地
		{"/a:b/c", false, ""},       // 冒号在斜杠后 = 本地
		{`C:\Users\x`, false, ""},   // Windows 盘符
		{"C:/Users/x", false, ""},   // Windows 盘符正斜杠
		{"rel:8080/x", true, "rel"}, // 首个冒号先于斜杠 = 远端（scp 语义）
	}
	for _, c := range cases {
		r, h, _ := splitSCPOperand(c.in)
		if r != c.remote || h != c.head {
			t.Errorf("splitSCPOperand(%q) = (%v,%q), want (%v,%q)", c.in, r, h, c.remote, c.head)
		}
	}
}
