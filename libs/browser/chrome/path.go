package chrome

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

func Resolve(configured string) (string, error) {
	explicit := configured
	if explicit == "" {
		explicit = os.Getenv("AIC_BROWSER_PATH")
	}
	if explicit != "" {
		p, err := executable(explicit)
		if err != nil {
			return "", fmt.Errorf("configured browser: %w", err)
		}
		return p, nil
	}
	candidates := []string{os.Getenv("AIC_BROWSER_DEFAULT_PATH"), "google-chrome", "chromium", "chromium-browser", "chrome"}
	if runtime.GOOS == "darwin" {
		candidates = append(candidates, "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", "/Applications/Chromium.app/Contents/MacOS/Chromium")
	}
	if runtime.GOOS == "windows" {
		for _, root := range []string{os.Getenv("PROGRAMFILES"), os.Getenv("PROGRAMFILES(X86)"), os.Getenv("LOCALAPPDATA")} {
			if root != "" {
				candidates = append(candidates, filepath.Join(root, "Google", "Chrome", "Application", "chrome.exe"))
			}
		}
	}
	for _, candidate := range candidates {
		if candidate != "" {
			if p, err := executable(candidate); err == nil {
				return p, nil
			}
		}
	}
	return "", fmt.Errorf("Chrome not found; configure browser_path or AIC_BROWSER_PATH")
}
func executable(path string) (string, error) {
	p, err := exec.LookPath(path)
	if err != nil {
		return "", err
	}
	return filepath.Abs(p)
}
