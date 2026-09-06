package host

import (
	"context"
	"os/exec"
	"testing"
)

func TestSplitSSHPort(t *testing.T) {
	cases := []struct {
		in, host string
		port     int
	}{
		{"example.com", "example.com", 0},
		{"example.com:2222", "example.com", 2222},
		{"root@example.com:1022", "root@example.com", 1022},
		{"root@example.com", "root@example.com", 0},
		{"::1", "::1", 0},                         // IPv6 多冒号不判端口
		{"example.com:abc", "example.com:abc", 0}, // 非数字端口原样
		{"example.com:0", "example.com:0", 0},     // 非法端口原样
	}
	for _, c := range cases {
		h, p := splitSSHPort(c.in)
		if h != c.host || p != c.port {
			t.Errorf("splitSSHPort(%q) = (%q,%d), want (%q,%d)", c.in, h, p, c.host, c.port)
		}
	}
}

// sshResolve 走 ssh -G（不联网、不解析 DNS——ssh -G 纯本地配置展开）。
func TestSSHResolve(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh binary not available")
	}
	host, port, err := sshResolve(context.Background(), "example.com", 0)
	if err != nil {
		t.Fatalf("sshResolve: %v", err)
	}
	if host != "example.com" || port != 22 {
		t.Fatalf("sshResolve(example.com) = %s:%d, want example.com:22", host, port)
	}
	// 端口覆盖随行
	_, port, err = sshResolve(context.Background(), "example.com", 2222)
	if err != nil || port != 2222 {
		t.Fatalf("sshResolve(-p 2222) port = %d, want 2222 (err=%v)", port, err)
	}
	// user@ 形态
	host, _, err = sshResolve(context.Background(), "root@example.com", 0)
	if err != nil || host != "example.com" {
		t.Fatalf("sshResolve(user@) host = %q, want example.com (err=%v)", host, err)
	}
}
