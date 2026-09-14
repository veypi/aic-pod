//go:build darwin

package fsauth

// systemCAPaths（darwin）：系统与常见包管理器（Homebrew arm64 / intel）的
// 公共 CA bundle 与证书目录。证书公开、非机密；清单内均为公共目录
// （私钥在 Keychain（已 deny）与 /etc/ssl/private（不入清单））。
func systemCAPaths() []string {
	return []string{
		"/etc/ssl/cert.pem",
		"/etc/ssl/certs/**",
		"/opt/homebrew/etc/openssl@*/cert.pem",
		"/opt/homebrew/etc/openssl@*/certs/**",
		"/opt/homebrew/etc/ca-certificates/**",
		"/usr/local/etc/openssl@*/cert.pem",
		"/usr/local/etc/openssl@*/certs/**",
		"/usr/local/etc/ca-certificates/**",
	}
}
