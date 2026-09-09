package host

import "testing"

func TestHasNonASCII(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"hello", false},
		{"hello world 123 !@#", false},
		{"", false},
		{"你好", true},
		{"abc你好", true},
		{"こんにちは", true},
		{"🎉", true},
	}
	for _, c := range cases {
		if got := hasNonASCII(c.in); got != c.want {
			t.Errorf("hasNonASCII(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestPasteModifier(t *testing.T) {
	if got := pasteModifier(); got != "cmd" && got != "ctrl" {
		t.Errorf("pasteModifier() = %q, want cmd or ctrl", got)
	}
}

// 非 ASCII type 走剪贴板粘贴（paste 字段），ASCII 仍走 type_text。
func TestMapCuaArgvTypeNonASCII(t *testing.T) {
	call, err := mapCuaArgv([]string{"type", "--text", "你好世界", "--pid", "1"})
	if err != nil {
		t.Fatal(err)
	}
	if call.paste != "你好世界" {
		t.Errorf("paste = %q, want 你好世界", call.paste)
	}
	if _, has := call.args["text"]; has {
		t.Error("paste 路径不应把 text 交给 type_text")
	}
	if call.args["pid"] != 1 {
		t.Errorf("pid = %v, want 1", call.args["pid"])
	}

	call2, err := mapCuaArgv([]string{"type", "--text", "hello", "--pid", "1"})
	if err != nil {
		t.Fatal(err)
	}
	if call2.paste != "" {
		t.Errorf("ASCII 文本不应走 paste，got %q", call2.paste)
	}
	if call2.tool != "type_text" || call2.args["text"] != "hello" {
		t.Errorf("ASCII 路径 tool=%q text=%v", call2.tool, call2.args["text"])
	}
}

// 混合文本（ASCII+非 ASCII）也走 paste——逐键合成遇到中文同样会被 IME 吞。
func TestMapCuaArgvTypeMixed(t *testing.T) {
	call, err := mapCuaArgv([]string{"type", "cmd 是 hello"})
	if err != nil {
		t.Fatal(err)
	}
	if call.paste != "cmd 是 hello" {
		t.Errorf("paste = %q", call.paste)
	}
}
