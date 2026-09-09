package host

// cua_ime.go —— 键盘类动作的输入法（IME）护栏（通用部分）。
//
// 背景（2026-09-09 Blender 实测）：中文 IME 激活时会拦截自动化按键——
// 搜狗拼音把 Shift 当"中英切换"，Shift+A 退化成裸 a（Blender 全选而非
// 弹出添加菜单）；字母键进候选窗不落字。自动化必须保证按键发生在英文
// 键盘布局下。
//
// 两条规则（所有平台默认生效）：
//  1. 键盘类动作（press_key/type_text）执行前：若当前输入源是 IME，切到
//     英文键盘布局（平台实现见 cua_ime_<goos>.go）；已是英文则零开销跳过；
//  2. 文本输入含非 ASCII（中文等）：不走逐键合成（会被 IME 吞或转候选），
//     改走剪贴板粘贴（clipboard_write + 粘贴热键）——见 cua.go 的 paste
//     分支与 runCuaPaste。
//
// 关闭：环境变量 AIC_CUA_IME_GUARD=0（用户需要保留自己的输入法状态时）。

import (
	"fmt"
	"os"
	"runtime"
	"sync"
)

// imeGuardDisabled 报告是否显式关闭 IME 护栏。
func imeGuardDisabled() bool {
	return os.Getenv("AIC_CUA_IME_GUARD") == "0"
}

// hasNonASCII 判断文本含非 ASCII 字符（中文/日文/emoji 等）。
// 这类文本经按键合成会被 IME 拦截或走候选流程，须改走剪贴板。
func hasNonASCII(s string) bool {
	for _, r := range s {
		if r > 127 {
			return true
		}
	}
	return false
}

// pasteModifier 返回粘贴热键修饰键：macOS cmd，其余平台 ctrl。
func pasteModifier() string {
	if runtime.GOOS == "darwin" {
		return "cmd"
	}
	return "ctrl"
}

// imeGuardMu 串行化检测/切换（并发动作不互相打架）。
var imeGuardMu sync.Mutex

// imeGuard 在键盘类动作前调用：确保系统输入法处于英文布局。
// 返回人读说明（空 = 无需动作；非空 = 已切换或失败原因，附在响应里）。
func imeGuard() string {
	if imeGuardDisabled() {
		return ""
	}
	imeGuardMu.Lock()
	defer imeGuardMu.Unlock()
	note, err := imeEnsureEnglish()
	if err != nil {
		return fmt.Sprintf("ime guard: %v", err)
	}
	return note
}
