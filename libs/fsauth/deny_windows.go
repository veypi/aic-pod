//go:build windows

package fsauth

// defaultDenyPaths（windows）：通用条目 + windows 平台特有——浏览器 profile、
// SAM 口令库。
func defaultDenyPaths() []string {
	return append(denyCommon(),
		"%LOCALAPPDATA%/Google/Chrome/User Data/**",
		"C:/Windows/System32/config/SAM",
	)
}
