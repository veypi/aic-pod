//go:build !darwin && !windows && !linux

package host

// 其他平台：IME 护栏为 no-op（动作照常执行；按键是否被输入法拦截由动作
// 结果暴露）。非 ASCII 文本仍走剪贴板粘贴（通用规则，平台无关）。

// imeEnsureEnglish 见 cua_ime.go 的契约。
func imeEnsureEnglish() (string, error) { return "", nil }
