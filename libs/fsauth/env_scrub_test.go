package fsauth

import "testing"

// TestScrubEnv：敏感变量剥离 + 子串误伤防护（MONKEY/TURKEY 保留）+ 工具链变量保留。
func TestScrubEnv(t *testing.T) {
	env := []string{
		"PATH=/usr/bin",
		"HOME=/home/u",
		"LANG=en_US.UTF-8",
		"NPM_TOKEN=abc",             // TOKEN 整词
		"AWS_SECRET_ACCESS_KEY=xyz", // SECRET/KEY 整词
		"GPG_PASSPHRASE=pw",         // PASSPHRASE 整词
		"MY_PASSWORD=pw",            // PASSWORD 整词
		"API_CREDENTIALS=cred",      // CREDENTIALS 整词
		"GIT_AUTH=auth",             // AUTH 整词
		"MONKEY=1",                  // 含 KEY 子串，单 token——保留
		"TURKEY=2",                  // 含 KEY 子串——保留
		"WHITEPASSION=3",            // 含 PASS 子串但非整词——保留
		"APIKEY=k",                  // 无边界连写：已知残余缺口——保留（文档化）
		"X=1",
	}
	got := ScrubEnv(env)
	dropped := map[string]bool{}
	for _, kv := range got {
		name, _, _ := cutEq(kv)
		dropped[name] = true
	}
	keep := []string{"PATH", "HOME", "LANG", "MONKEY", "TURKEY", "WHITEPASSION", "APIKEY", "X"}
	lose := []string{"NPM_TOKEN", "AWS_SECRET_ACCESS_KEY", "GPG_PASSPHRASE", "MY_PASSWORD", "API_CREDENTIALS", "GIT_AUTH"}
	for _, k := range keep {
		if !dropped[k] {
			t.Errorf("env %s should be kept", k)
		}
	}
	for _, l := range lose {
		if dropped[l] {
			t.Errorf("env %s should be scrubbed", l)
		}
	}
	if len(got) != len(keep) {
		t.Errorf("kept %d envs, want %d: %v", len(got), len(keep), got)
	}
}

func cutEq(s string) (string, string, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '=' {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}
