//go:build linux

package fsauth

// defaultDenyPaths（linux）：通用条目 + linux 平台特有——系统口令/影子文件、
// sudoers/sshd 配置、内核内存设备、docker 套接字、浏览器 profile。
func defaultDenyPaths() []string {
	return append(denyCommon(),
		"/etc/shadow", "/etc/gshadow", "/etc/sudoers*", "/etc/ssh/**",
		"/dev/mem", "/dev/kmem", "/var/run/docker.sock",
		"~/.config/google-chrome/**",
	)
}
