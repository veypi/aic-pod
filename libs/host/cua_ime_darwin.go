//go:build darwin

package host

// macOS 输入法护栏实现。
//
// 不用 cgo/TIS（Carbon）：项目以 CGO_ENABLED=0 构建（Makefile），且 TIS 的
// 纯 Go 调用需要 FFI 依赖。改用系统命令组合：
//   - 读：defaults read com.apple.HIToolbox AppleSelectedInputSources
//     （plist 文本；含 "Input Mode" 即 IME 输入源激活）；
//   - 切：osascript——临时激活 Finder（它不拦截 Ctrl+Space 这个系统级
//     "选择上一个输入源"热键），发一次 Ctrl+Space，再切回原前台 app。
//     不能直接对前台 app 发：Blender 等把 Ctrl+Space 绑给了"最大化区域"，
//     事件会被 app 吃掉（2026-09-09 实测）。
//
// 实测（2026-09-09）：搜狗拼音激活时本流程可切回 ABC；已在英文布局时
// 只花一次 defaults 读取（~15ms）直接跳过。

import (
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const imeDarwinDomain = "com.apple.HIToolbox"

// imeDarwinSelected 读当前选中的输入源（plist 文本）。
func imeDarwinSelected() (string, error) {
	out, err := exec.Command("defaults", "read", imeDarwinDomain, "AppleSelectedInputSources").Output()
	if err != nil {
		return "", fmt.Errorf("defaults read AppleSelectedInputSources: %w", err)
	}
	return string(out), nil
}

// imeDarwinIsEnglish：选中源里没有 "Input Mode"（IME 输入源）即视为英文/拉丁布局。
func imeDarwinIsEnglish(s string) bool {
	return !strings.Contains(s, "Input Mode")
}

// imeDarwinSourceName 提取当前输入源可读名（日志用）。
func imeDarwinSourceName(s string) string {
	for _, key := range []string{`"Input Mode" = "`, `"KeyboardLayout Name" = `} {
		if i := strings.Index(s, key); i >= 0 {
			rest := s[i+len(key):]
			end := strings.IndexAny(rest, "\";\n")
			if end > 0 {
				return strings.TrimSpace(rest[:end])
			}
		}
	}
	return "unknown"
}

// imeDarwinSwitch 经 osascript 切到上一个输入源（Ctrl+Space），并恢复原前台 app。
func imeDarwinSwitch() error {
	const script = `tell application "System Events"
	set frontName to name of first application process whose frontmost is true
	tell application "Finder" to activate
	delay 0.6
	key code 49 using control down
	delay 0.5
	set frontmost of application process frontName to true
end tell`
	out, err := exec.Command("osascript", "-e", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("osascript: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// imeEnsureEnglish 见 cua_ime.go 的契约：确保当前输入法是英文键盘布局。
func imeEnsureEnglish() (string, error) {
	cur, err := imeDarwinSelected()
	if err != nil {
		return "", err
	}
	if imeDarwinIsEnglish(cur) {
		return "", nil
	}
	from := imeDarwinSourceName(cur)
	if err := imeDarwinSwitch(); err != nil {
		return "", err
	}
	time.Sleep(250 * time.Millisecond)
	after, aerr := imeDarwinSelected()
	if aerr == nil && !imeDarwinIsEnglish(after) {
		return "", fmt.Errorf("still %s after switch", imeDarwinSourceName(after))
	}
	return fmt.Sprintf("switched input source %s → English layout", from), nil
}
