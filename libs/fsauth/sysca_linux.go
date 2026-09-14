//go:build linux

package fsauth

// systemCAPaths（linux）：发行版公共 CA 目录（Debian/Ubuntu、RHEL 系）。
// 注：linux deny 实例化对 ** 开头模式 $HOME 根级锚定，系统路径本不在 deny
// 覆盖内——本清单为对齐/未来防护（当前 no-op 语义）。
func systemCAPaths() []string {
	return []string{
		"/etc/ssl/certs/**",
		"/etc/pki/**",
		"/usr/share/ca-certificates/**",
	}
}
