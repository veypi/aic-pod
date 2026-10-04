package host

import (
	"strings"
	"testing"
)

// ProgramData 必须透传给 OpenSSH 客户端：Win32-OpenSSH 缺它会吞掉全部诊断输出
// （2026-10-05 win 实机：连接失败只有 exit 255、stdout/stderr 全空）。
func TestSSHClientEnvPassthrough(t *testing.T) {
	t.Setenv("ProgramData", `C:\ProgramData`)
	t.Setenv("AIC_UNRELATED_PROBE", "leak")
	t.Setenv("SYSTEMROOT", `C:\WINDOWS`)
	env := sshClientEnv(`C:\Users\v`)

	has := func(want string) bool {
		for _, e := range env {
			if e == want {
				return true
			}
		}
		return false
	}
	if !has(`ProgramData=C:\ProgramData`) {
		t.Error("ProgramData 必须在白名单里（否则 win 上 ssh 失败静默）")
	}
	if !has(`SYSTEMROOT=C:\WINDOWS`) {
		t.Error("SYSTEMROOT 必须透传")
	}
	if !has(`HOME=C:\Users\v`) || !has("SSH_ASKPASS_REQUIRE=never") {
		t.Errorf("HOME/SSH_ASKPASS_REQUIRE 必须固定写入：%v", env)
	}
	for _, e := range env {
		if strings.HasPrefix(e, "AIC_UNRELATED_PROBE=") {
			t.Error("非白名单变量不得透传（脚本环境必须被 ReplaceEnv 隔绝）")
		}
	}
}

// Windows 的 user.Current() 返回 "DOMAIN\user"，默认 SSH 用户名必须取裸用户名。
func TestDefaultSSHUser(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`DESKTOP-6QGJPLH\v`, "v"},
		{`v`, "v"},
		{``, ``},
		{`\\v`, "v"},
	} {
		if got := defaultSSHUser(c.in); got != c.want {
			t.Errorf("defaultSSHUser(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}
