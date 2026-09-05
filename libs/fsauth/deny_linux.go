//go:build linux

package fsauth

// defaultDenyPaths（linux）：通用条目 + linux 平台特有——系统口令/影子文件、
// sudoers/sshd 配置、内核内存设备、钥匙串、浏览器 profile、容器运行时数据区。
func defaultDenyPaths() []string {
	return append(denyCommon(),
		"/etc/shadow", "/etc/gshadow", "/etc/sudoers*", "/etc/ssh/**",
		"/dev/mem", "/dev/kmem",
		// 钥匙串与浏览器 profile（Firefox key4.db、Chrome 系 Cookies）
		"~/.local/share/keyrings/**",
		"~/.mozilla/**",
		"~/.config/google-chrome/**",
		"~/.config/google-chrome-beta/**",
		"~/.config/chromium/**",
		"~/.config/microsoft-edge/**",
		"~/.config/BraveSoftware/**",
		"~/.config/Vivaldi/**",
		"~/.config/opera/**",
		// podman machine socket 数据区
		"~/.local/share/containers/**",
	)
}
