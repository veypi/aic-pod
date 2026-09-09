//go:build linux

package host

// Linux 输入法护栏实现（尽力而为：输入法栈差异大，X11/ibus/fcitx/Wayland
// 各有机制）。
//
// 策略（按可用性降级）：
//  1. X11/XKB：setxkbmap -query 读当前布局，非 us 则 setxkbmap us；
//  2. GNOME（含 Wayland）：gsettings 读/写输入源 current（默认第一个源当英文）；
//  3. 都不识别：no-op（返回空说明，不阻断动作——按键是否被 IME 拦截由
//     动作结果暴露）。
//
// 注意：XKB 布局与 IBus/Fcitx 的"输入法"是两层，前者管键盘映射，后者管
// 候选转换。本实现主要修正前者；后者（如 fcitx5 的中英态）无法从命令行
// 稳定切换，保留给用户自行配置。

import (
	"fmt"
	"os/exec"
	"strings"
)

// imeEnsureEnglish 见 cua_ime.go 的契约。
func imeEnsureEnglish() (string, error) {
	if note, err, ok := imeLinuxXKB(); ok {
		return note, err
	}
	if note, err, ok := imeLinuxGNOME(); ok {
		return note, err
	}
	return "", nil
}

// imeLinuxXKB：X11 下用 setxkbmap。
func imeLinuxXKB() (string, error, bool) {
	out, err := exec.Command("setxkbmap", "-query").Output()
	if err != nil {
		return "", nil, false
	}
	s := string(out)
	layout := ""
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "layout:") {
			layout = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "layout:"))
		}
	}
	if layout == "" {
		return "", nil, false
	}
	first := strings.Split(layout, ",")[0]
	if first == "us" {
		return "", nil, true
	}
	if err := exec.Command("setxkbmap", "us").Run(); err != nil {
		return "", fmt.Errorf("setxkbmap us: %w", err), true
	}
	return fmt.Sprintf("switched XKB layout %s → us", first), nil, true
}

// imeLinuxGNOME：GNOME 输入源列表（Wayland 下 XKB 查询通常不可用）。
func imeLinuxGNOME() (string, error, bool) {
	const schema = "org.gnome.desktop.input-sources"
	out, err := exec.Command("gsettings", "get", schema, "sources").Output()
	if err != nil {
		return "", nil, false
	}
	srcs := string(out)
	if !strings.Contains(srcs, "('xkb', 'us')") {
		return "", nil, false // 没有英文源，不擅自改动
	}
	cur, err := exec.Command("gsettings", "get", schema, "current").Output()
	if err != nil {
		return "", nil, false
	}
	idx := strings.Index(srcs, "('xkb', 'us')")
	// 计算 us 在列表中的序号：按 ")," 分组粗算（输入源条目数很少）。
	pos := 0
	{
		inner := srcs
		for {
			open := strings.Index(inner, "(")
			if open < 0 {
				break
			}
			if open >= idx {
				break
			}
			pos++
			next := strings.Index(inner[open:], "),")
			if next < 0 {
				break
			}
			inner = inner[open+next+2:]
		}
	}
	if strings.TrimSpace(cur) == fmt.Sprintf("uint32 %d", pos) {
		return "", nil, true
	}
	if err := exec.Command("gsettings", "set", schema, "current", fmt.Sprintf("%d", pos)).Run(); err != nil {
		return "", fmt.Errorf("gsettings set input-sources current: %w", err), true
	}
	return fmt.Sprintf("switched GNOME input source → index %d (us)", pos), nil, true
}
