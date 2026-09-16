package policy

import "testing"

func TestStringRulesAndDenyPrecedence(t *testing.T) {
	for _, tc := range []struct {
		raw, path string
		ro        bool
	}{
		{"/work/**", "/work/**", false}, {"ro:/skills/*/**", "/skills/*/**", true}, {"ro:C:/public/**", "C:/public/**", true}, {"C:/work", "C:/work", false},
	} {
		got, err := ParseFSAllow(tc.raw)
		if err != nil || got.Path != tc.path || got.ReadOnly != tc.ro {
			t.Fatalf("parse %q: %+v / %v", tc.raw, got, err)
		}
	}
	for _, bad := range []string{"", "ro:", "ro:  ", "/a\x00b"} {
		if _, err := ParseFSAllow(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
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
