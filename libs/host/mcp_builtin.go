package host

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/veypi/aic-pod/libs/mcpx"
)

// DefaultIdleTimeout 是内置 browser/CUA 服务的默认空闲有效期。
const DefaultIdleTimeout = "30m"

// Defaults launch upstream executables directly. Owner entries replace them in
// full; this code neither registers tools nor changes their arguments/results.
// 内置服务默认 30m 空闲有效期（idle_timeout），避免浏览器/桌面驱动长期常驻；
// 所有者显式配置同名项时整体替换默认项，包括该默认有效期。
func mcpServers(configured map[string]mcpx.Config, workDir string) (map[string]mcpx.Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	bundle := func(env, name string) string {
		if dir := os.Getenv(env); dir != "" {
			return dir
		}
		dir := filepath.Join(filepath.Dir(executable), name)
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			return dir
		}
		return ""
	}
	browser := mcpx.Config{Command: "agent-browser", Args: []string{"mcp", "--tools", "core,tabs"}, NoSandbox: true, Cwd: workDir, IdleTimeout: DefaultIdleTimeout,
		Env: map[string]string{
			"AGENT_BROWSER_SESSION":    "aic",
			"AGENT_BROWSER_SOCKET_DIR": filepath.Join(home, ".aic", "browser", "runtime"),
			"AGENT_BROWSER_PROFILE":    filepath.Join(home, ".aic", "browser", "profile"),
			"AGENT_BROWSER_HEADED":     "false",
		}}
	if explicit := os.Getenv("AIC_AGENT_BROWSER_PATH"); explicit != "" {
		browser.Command = explicit
	} else if root := bundle("AIC_AGENT_BROWSER_BUNDLE_DIR", "agent-browser"); root != "" {
		name := "agent-browser"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		browser.Command = filepath.Join(root, name)
	}
	chrome := os.Getenv("AIC_BROWSER_PATH")
	if chrome == "" {
		if root := bundle("AIC_BROWSER_BUNDLE_DIR", "browser"); root != "" {
			platform, arch, name := runtime.GOOS, runtime.GOARCH, "chrome"
			if arch == "amd64" {
				arch = "x64"
			}
			switch platform {
			case "darwin":
				name = "Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing"
			case "windows":
				platform, name = "win32", "chrome.exe"
			}
			chrome = filepath.Join(root, platform+"-"+arch, filepath.FromSlash(name))
		}
	}
	if chrome != "" {
		browser.Env["AGENT_BROWSER_EXECUTABLE_PATH"] = chrome
	}
	cua := mcpx.Config{Command: "cua-driver", Args: []string{"mcp"}, NoSandbox: true, IdleTimeout: DefaultIdleTimeout,
		Env: map[string]string{"CUA_DRIVER_RS_UPDATE_CHECK": "false", "CUA_DRIVER_RS_TELEMETRY_ENABLED": "false"}}
	if explicit := os.Getenv("AIC_CUA_DRIVER_PATH"); explicit != "" {
		cua.Command = explicit
	} else if root := bundle("AIC_CUA_BUNDLE_DIR", "cua"); root != "" {
		name := "cua-driver"
		switch runtime.GOOS {
		case "darwin":
			name = "CuaDriver.app/Contents/MacOS/cua-driver"
		case "windows":
			name = "cua-driver.exe"
		}
		cua.Command = filepath.Join(root, filepath.FromSlash(name))
	} else if _, err := exec.LookPath(cua.Command); err != nil && runtime.GOOS == "darwin" {
		cua.Command = "/Applications/CuaDriver.app/Contents/MacOS/cua-driver"
	}
	servers := map[string]mcpx.Config{"browser": browser, "cua": cua}
	for name, config := range configured {
		servers[name] = config
	}
	return servers, nil
}
