package chrome

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// Chrome 探测候选链（desktop 只注入目录级 env，可执行文件解析全部在本文件）：
//  1. AIC_BROWSER_PATH（或调用方 configured）——显式用户覆盖，最高优先；
//  2. AIC_BROWSER_BUNDLE_DIR——打包器提示：目录内含 Chrome for Testing
//     （{platform}-{arch}/ 布局，desktop vendor/browser → resources/browser）；
//  3. 系统候选（macOS .app / Windows PROGRAMFILES 系 / Linux PATH 名）；
//  4. 全部缺失 → 报错提示 set AIC_BROWSER_PATH。
func Resolve(configured string) (string, error) {
	return resolve(configured, runtime.GOOS, runtime.GOARCH, os.Getenv)
}

func resolve(configured, goos, goarch string, getenv func(string) string) (string, error) {
	explicit := configured
	if explicit == "" {
		explicit = getenv("AIC_BROWSER_PATH")
	}
	if explicit != "" {
		p, err := executable(explicit)
		if err != nil {
			return "", fmt.Errorf("configured browser: %w", err)
		}
		return p, nil
	}
	if dir := getenv("AIC_BROWSER_BUNDLE_DIR"); dir != "" {
		if p, err := executable(bundleExecutable(dir, goos, goarch)); err == nil {
			return p, nil
		}
	}
	for _, candidate := range systemCandidates(goos, getenv) {
		if p, err := executable(candidate); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("Chrome not found; set AIC_BROWSER_PATH")
}

// bundleExecutable Chrome for Testing 平台/架构目录内的可执行文件路径
// （布局与 desktop sync-browser 同步口径一致：{platform}-{arch}/，platform
// 取 darwin|win32|linux，arch 取 x64|arm64）。
func bundleExecutable(root, goos, goarch string) string {
	platform := goos
	if goos == "windows" {
		platform = "win32"
	}
	arch := goarch
	if goarch == "amd64" {
		arch = "x64"
	}
	name := "chrome"
	switch goos {
	case "darwin":
		name = "Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing"
	case "windows":
		name = "chrome.exe"
	}
	return filepath.Join(root, platform+"-"+arch, filepath.FromSlash(name))
}

// systemCandidates 各平台系统安装候选（绝对路径与 PATH 名混排，executable 统一解析）。
func systemCandidates(goos string, getenv func(string) string) []string {
	switch goos {
	case "darwin":
		return []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		}
	case "windows":
		var out []string
		for _, root := range []string{getenv("PROGRAMFILES"), getenv("PROGRAMFILES(X86)"), getenv("LOCALAPPDATA")} {
			if root != "" {
				out = append(out, filepath.Join(root, "Google", "Chrome", "Application", "chrome.exe"))
			}
		}
		return out
	default: // linux 及其余 unix：PATH 名
		return []string{"google-chrome", "chromium", "chromium-browser", "chrome"}
	}
}

func executable(path string) (string, error) {
	p, err := exec.LookPath(path)
	if err != nil {
		return "", err
	}
	return filepath.Abs(p)
}
