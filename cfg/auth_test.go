package cfg

import (
	"errors"
	"testing"
)

// TestAuthCfgValidateReportsField 锁定授权校验的唯一入口：失败字段由
// AuthFieldError 标注，设置面据此回显到具体输入项。
func TestAuthCfgValidateReportsField(t *testing.T) {
	for _, tc := range []struct {
		name  string
		auth  AuthCfg
		field string
	}{
		{"policy", AuthCfg{FsPolicy: "typo"}, "fs_policy"},
		{"fs", AuthCfg{FsRules: []string{"rw:**"}}, "fs_rules"},
		{"exec", AuthCfg{ExecRules: []string{"git*"}}, "exec_rules"},
		{"net", AuthCfg{NetRules: []string{"*:443"}}, "net_rules"},
		{"ssh", AuthCfg{SshRules: []string{"*:22"}}, "ssh_rules"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.auth.Validate()
			var field *AuthFieldError
			if !errors.As(err, &field) || field.Field != tc.field {
				t.Fatalf("Validate() = %v, want field %q", err, tc.field)
			}
		})
	}
	if err := (AuthCfg{}).Validate(); err != nil {
		t.Fatalf("empty auth config must validate: %v", err)
	}
}

// TestSettingsApplyRejectsInvalidRulesWithField 设置面只校验本次提交的字段，
// 并把底层 AuthFieldError 映射成 InvalidArg（不因文件里已有的坏内容拒绝保存）。
func TestSettingsApplyRejectsInvalidRulesWithField(t *testing.T) {
	isolateConfigDir(t)
	rules := []string{"rw:**"}
	err := (&SettingsUpdate{FsRules: &rules}).Apply()
	var invalid *InvalidArg
	if !errors.As(err, &invalid) || invalid.Field != "fs_rules" {
		t.Fatalf("Apply() = %v, want fs_rules InvalidArg", err)
	}
}

func TestAuthCfgValidatesUpstreamProxy(t *testing.T) {
	for _, raw := range []string{"", "http://127.0.0.1:7897", "socks5://127.0.0.1:1080", "socks5h://proxy.example:1080"} {
		if err := (AuthCfg{NetUpstreamProxy: raw}).Validate(); err != nil {
			t.Errorf("valid net_upstream_proxy %q rejected: %v", raw, err)
		}
	}
	for _, raw := range []string{
		"ftp://127.0.0.1:21",
		"http://127.0.0.1:99999",
		"http://127.0.0.1:7897/not-a-proxy-path",
	} {
		t.Run(raw, func(t *testing.T) {
			err := (AuthCfg{NetUpstreamProxy: raw}).Validate()
			if err == nil {
				t.Fatalf("invalid net_upstream_proxy %q passed AuthCfg.Validate", raw)
			}
			field, ok := err.(*AuthFieldError)
			if !ok || field.Field != "net_upstream_proxy" {
				t.Fatalf("error=%T %+v, want net_upstream_proxy field error", err, err)
			}
		})
	}
}
