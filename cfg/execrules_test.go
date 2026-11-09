package cfg

import "testing"

// open 语义回归：带空格路径（Windows Program Files）在 open 模式下必须放行，
// 不能被身份合法性前置闸结构性拒绝（2026-11-08 win Git curl 实测翻车）。
func TestCommandAllowedOpenSkipsIdentityGate(t *testing.T) {
	spaced := "/c/Program Files/Git/mingw64/bin/curl.exe"
	cases := []struct {
		name string
		mode string
		rows []string
		ids  []string
		want bool
	}{
		{"open/spaced-path", "open", nil, []string{spaced, spaced}, true},
		{"open/spaced-name-only", "open", nil, []string{spaced}, true},
		{"open/bare-name", "open", nil, []string{"python"}, true},
		{"open/explicit-deny-still-wins", "open", []string{"deny:curl"}, []string{"curl", "/usr/bin/curl"}, false},
		{"open/deny-other-command", "open", []string{"deny:rm"}, []string{"curl"}, true},
		{"open/allow-row", "open", []string{"allow:foo"}, []string{"foo"}, true},
		{"deny/spaced-path-rejected", "deny", []string{"allow:" + "/usr/bin/curl"}, []string{spaced}, false},
		{"deny/valid-allow", "deny", []string{"allow:curl"}, []string{"curl"}, true},
		{"deny/no-match", "deny", nil, []string{"curl"}, false},
		{"invalid-mode", "whatever", nil, []string{"curl"}, false},
		{"empty-mode", "", nil, []string{"curl"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CommandAllowed(tc.mode, tc.rows, tc.ids...); got != tc.want {
				t.Fatalf("CommandAllowed(%q, %v, %v) = %v, want %v", tc.mode, tc.rows, tc.ids, got, tc.want)
			}
		})
	}
}

// 规则表语法不变：带空格的身份依旧写不进规则（配置编译期拒绝）。
func TestParseExecRuleRejectsSpaces(t *testing.T) {
	if _, _, err := ParseExecRule("allow:/c/Program Files/Git/mingw64/bin/curl.exe"); err == nil {
		t.Fatal("rule with spaced path must stay invalid")
	}
	if err := ValidateCommandName("/c/Program Files/x.exe"); err == nil {
		t.Fatal("ValidateCommandName must keep rejecting spaces")
	}
}
