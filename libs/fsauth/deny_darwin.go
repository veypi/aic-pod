//go:build darwin

package fsauth

// defaultDenyPaths（darwin）：通用条目 + mac 平台特有——系统钥匙串、
// 系统级 cookie 库、浏览器 profile（密钥库）、密码管理器数据、容器/虚拟机
// 运行时数据区、系统口令文件。
func defaultDenyPaths() []string {
	return append(denyCommon(),
		// 系统钥匙串（login.keychain-db 等：文件本身加密，但可拖走离线爆破）
		"~/Library/Keychains/**", "/Library/Keychains/**",
		// 系统级 cookie 库（Safari + NSHTTPCookieStorage 共用）
		"~/Library/Cookies/**",
		// 浏览器 profile：Chrome 系 Cookies/Local State、Firefox key4.db/logins.json
		"~/Library/Application Support/Google/Chrome/**",
		"~/Library/Application Support/Google/Chrome Canary/**",
		"~/Library/Application Support/Chromium/**",
		"~/Library/Application Support/Microsoft Edge/**",
		"~/Library/Application Support/BraveSoftware/**",
		"~/Library/Application Support/Vivaldi/**",
		"~/Library/Application Support/Arc/**",
		"~/Library/Application Support/com.operasoftware.Opera/**",
		"~/Library/Application Support/Firefox/**",
		// 密码管理器数据（加密库不出本机）
		"~/Library/Group Containers/2BUA8C4S2C.com.agilebits/**",
		"~/Library/Application Support/KeePassXC/**",
		"~/Library/Application Support/Bitwarden/**",
		// 容器/虚拟机运行时：docker/podman socket、OrbStack sconssh（直通 VM
		// 的 ssh）/vmcontrol、colima/lima VM 数据；~/OrbStack 为 OrbStack VM
		// 文件共享挂载（含 VM home）
		"~/.orbstack/**", "~/OrbStack/**", "~/.colima/**", "~/.lima/**", "~/.rd/**",
		"~/.local/share/containers/**",
		"/etc/master.passwd", "/etc/sudoers*", "/etc/ssh/**",
	)
}
