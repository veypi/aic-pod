package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseFSRule(t *testing.T) {
	for _, tc := range []struct {
		raw, effect, pattern string
	}{
		{"deny:**/.ssh/**", "deny", "**/.ssh/**"},
		{"ro:~/.ssh/known_hosts", "ro", "~/.ssh/known_hosts"},
		{"rw:~/.cache/**", "rw", "~/.cache/**"},
		{"rw:C:/docs/**", "rw", "C:/docs/**"}, // 白名单前缀只剥 rw:，盘符按字面保留
		{" deny:/etc/ssh/** ", "deny", "/etc/ssh/**"},
	} {
		eff, pat, err := ParseFSRule(tc.raw)
		if err != nil || eff != tc.effect || pat != tc.pattern {
			t.Fatalf("parse %q: %q %q / %v", tc.raw, eff, pat, err)
		}
	}
	// 缺前缀 / 空模式 / 非法字符：报错不静默。
	for _, bad := range []string{"", "ro:", "/work/**", "C:/work", "allow:/a", "rw:", "rw:/a\x00b"} {
		if _, _, err := ParseFSRule(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestParseFSRuleGlobalBan(t *testing.T) {
	// 字面全域：全效果禁写（指向 fs_policy）。
	for _, bad := range []string{"rw:**", "rw:/", "rw:/**", "ro:**", "deny:**", "deny:/", "deny:/**"} {
		if _, _, err := ParseFSRule(bad); err == nil {
			t.Fatalf("accepted global rule %q", bad)
		}
	}
	// 放行类另禁家目录根与整盘根；deny 不受此限。
	for _, bad := range []string{"rw:~", "rw:~/", "rw:~/**", "ro:~", "rw:C:", "rw:C:/", "rw:C:/**", "ro:c:/**"} {
		if _, _, err := ParseFSRule(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		h := filepath.ToSlash(home)
		for _, bad := range []string{"rw:" + h, "rw:" + h + "/", "rw:" + h + "/**"} {
			if _, _, err := ParseFSRule(bad); err == nil {
				t.Fatalf("accepted expanded home rule %q", bad)
			}
		}
	}
	for _, ok := range []string{"deny:~", "deny:~/**", "deny:C:/**", "rw:~/.cache/**", "ro:/etc"} {
		if _, _, err := ParseFSRule(ok); err != nil {
			t.Fatalf("rejected %q: %v", ok, err)
		}
	}
}

func TestParseTargetRule(t *testing.T) {
	allow, e, err := ParseTargetRule("allow:github.com:22")
	if err != nil || !allow || e.String() != "github.com:22" {
		t.Fatalf("parse allow: %v %q %v", allow, e.String(), err)
	}
	allow, e, err = ParseTargetRule("deny:169.254.169.254")
	if err != nil || allow || e.String() != "169.254.169.254:*" {
		t.Fatalf("parse deny: %v %q %v", allow, e.String(), err)
	}
	// 缺前缀与全域：报错（ParseEntry 本身拒绝通配 host）。
	for _, bad := range []string{"", "example.com:443", "allow:*", "allow:*:*", "deny:*", "allow:*.corp.com:443", "allow:bad:0"} {
		if _, _, err := ParseTargetRule(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestValidateFSGrantTarget(t *testing.T) {
	if err := ValidateFSGrantTarget("/"); err == nil {
		t.Fatal("accepted /")
	}
	if home, err := os.UserHomeDir(); err == nil {
		if err := ValidateFSGrantTarget(home); err == nil {
			t.Fatal("accepted home root")
		}
		if err := ValidateFSGrantTarget(filepath.Join(home, "project")); err != nil {
			t.Fatalf("rejected home subdir: %v", err)
		}
	}
	for _, bad := range []string{"C:", "C:/", "c:\\"} {
		if err := ValidateFSGrantTarget(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	if err := ValidateFSGrantTarget("/tmp/x"); err != nil {
		t.Fatalf("rejected /tmp/x: %v", err)
	}
}

func TestValidateLists(t *testing.T) {
	if err := ValidateFSRules([]string{"deny:~/**/*.pem", "rw:~/.cache/**"}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateFSRules([]string{"rw:**"}); err == nil || !strings.Contains(err.Error(), "rw:**") {
		t.Fatalf("global rule not reported: %v", err)
	}
	if err := ValidateTargetRules([]string{"deny:169.254.169.254:*", "allow:git.corp.com:443"}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTargetRules([]string{"allow:*"}); err == nil {
		t.Fatal("accepted allow:*")
	}
}

func TestCommandAllowed(t *testing.T) {
	if CommandAllowed("open", []string{"bash"}, []string{"*"}, "bash") {
		t.Fatal("allow overrode deny")
	}
	if CommandAllowed("deny", nil, nil, "git") {
		t.Fatal("missing command allowed")
	}
	if !CommandAllowed("deny", nil, []string{"git"}, "git") {
		t.Fatal("explicit command denied")
	}
	for _, bad := range []string{"git*", "/bin/sh", "bash -c", "git?"} {
		if ValidateExec([]string{bad}) == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
