//go:build darwin

package host

import "testing"

const imeDarwinSogouOut = `(
        {
        "Bundle ID" = "com.apple.PressAndHold";
        InputSourceKind = "Non Keyboard Input Method";
    },
        {
        "Bundle ID" = "com.sogou.inputmethod.sogou";
        "Input Mode" = "com.sogou.inputmethod.pinyin";
        InputSourceKind = "Input Mode";
    }
)`

const imeDarwinABCOut = `(
        {
        "Bundle ID" = "com.apple.PressAndHold";
        InputSourceKind = "Non Keyboard Input Method";
    },
        "KeyboardLayout Name" = ABC;
)`

func TestIMEDarwinIsEnglish(t *testing.T) {
	if imeDarwinIsEnglish(imeDarwinSogouOut) {
		t.Error("搜狗拼音激活应判为非英文（需要切换）")
	}
	if !imeDarwinIsEnglish(imeDarwinABCOut) {
		t.Error("ABC 布局应判为英文（无需动作）")
	}
}

func TestIMEDarwinSourceName(t *testing.T) {
	if got := imeDarwinSourceName(imeDarwinSogouOut); got != "com.sogou.inputmethod.pinyin" {
		t.Errorf("sogou source name = %q", got)
	}
	if got := imeDarwinSourceName(imeDarwinABCOut); got != "ABC" {
		t.Errorf("ABC source name = %q", got)
	}
	if got := imeDarwinSourceName("garbage"); got != "unknown" {
		t.Errorf("unknown source name = %q", got)
	}
}
