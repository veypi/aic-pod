//go:build windows

package host

// Windows 输入法护栏实现（syscall 直调 user32，无第三方依赖）。
//
//   - 读：GetForegroundWindow → GetWindowThreadProcessId → GetKeyboardLayout，
//     取 HKL 低 16 位语言 ID；0x0409 = en-US（英文）视为无需处理；
//   - 切：LoadKeyboardLayout("00000409") + PostMessage(WM_INPUTLANGCHANGEREQUEST)
//     请求前台窗口切到英文布局。
//
// 说明：Windows 的 IME 与键盘布局绑定，切到 en-US 布局即等价于 macOS 侧
// 切到 ABC——组合键（Shift+A 等）不再被 IME 拦截。

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	imeUser32                   = syscall.NewLazyDLL("user32.dll")
	imeGetForegroundWindow      = imeUser32.NewProc("GetForegroundWindow")
	imeGetWindowThreadProcessID = imeUser32.NewProc("GetWindowThreadProcessId")
	imeGetKeyboardLayout        = imeUser32.NewProc("GetKeyboardLayout")
	imePostMessageW             = imeUser32.NewProc("PostMessageW")
	imeLoadKeyboardLayoutW      = imeUser32.NewProc("LoadKeyboardLayoutW")
)

const (
	imeWMInputLangChangeRequest = 0x0050
	imeKLFActivate              = 0x0001
	imeLangENUS                 = 0x0409
)

// imeEnsureEnglish 见 cua_ime.go 的契约。
func imeEnsureEnglish() (string, error) {
	hwnd, _, _ := imeGetForegroundWindow.Call()
	if hwnd == 0 {
		return "", fmt.Errorf("no foreground window")
	}
	tid, _, _ := imeGetWindowThreadProcessID.Call(hwnd, 0)
	if tid == 0 {
		return "", fmt.Errorf("GetWindowThreadProcessId failed")
	}
	hkl, _, _ := imeGetKeyboardLayout.Call(tid)
	langID := uint16(hkl & 0xFFFF)
	if langID == imeLangENUS {
		return "", nil
	}
	usName, err := syscall.UTF16PtrFromString("00000409")
	if err != nil {
		return "", err
	}
	usHKL, _, callErr := imeLoadKeyboardLayoutW.Call(uintptr(unsafe.Pointer(usName)), imeKLFActivate)
	if usHKL == 0 {
		return "", fmt.Errorf("LoadKeyboardLayout(en-US): %v", callErr)
	}
	imePostMessageW.Call(hwnd, imeWMInputLangChangeRequest, 0, usHKL)
	return fmt.Sprintf("switched input language 0x%04x → en-US", langID), nil
}
