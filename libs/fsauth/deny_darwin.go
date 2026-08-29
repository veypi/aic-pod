//go:build darwin

package fsauth

// defaultDenyPaths（darwin）：通用条目 + mac 平台特有——浏览器 profile、
// 系统口令文件（master.passwd）、sudoers/sshd 配置、docker 套接字。
func defaultDenyPaths() []string {
	return append(denyCommon(),
		"~/Library/Application Support/Google/Chrome/**",
		"/etc/master.passwd", "/etc/sudoers*", "/etc/ssh/**",
		"/var/run/docker.sock",
	)
}
