//go:build windows

package fsauth

// defaultDenyPaths（windows）：通用条目 + windows 平台特有——浏览器 profile、
// 密码管理器数据、SAM 口令库。
func defaultDenyPaths() []string {
	return append(denyCommon(),
		"%LOCALAPPDATA%/Google/Chrome/User Data/**",
		"%LOCALAPPDATA%/Google/Chrome SxS/User Data/**",
		"%LOCALAPPDATA%/Chromium/User Data/**",
		"%LOCALAPPDATA%/Microsoft/Edge/User Data/**",
		"%LOCALAPPDATA%/BraveSoftware/Brave-Browser/User Data/**",
		"%LOCALAPPDATA%/Vivaldi/User Data/**",
		"%APPDATA%/Mozilla/Firefox/**",
		"%APPDATA%/KeePassXC/**",
		"%LOCALAPPDATA%/1Password/**",
		"C:/Windows/System32/config/SAM",
	)
}
