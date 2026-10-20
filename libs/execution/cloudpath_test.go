package execution

import (
	"strings"
	"testing"
)

func TestApplyAliasLongestPrefix(t *testing.T) {
	aliases := map[string]string{"/tmp": "/u/u1/s/tmp", "/tmp/cache": "/u/u1/s/cache"}
	for _, tc := range []struct{ in, want string }{
		{"/tmp", "/u/u1/s/tmp"},
		{"/tmp/a", "/u/u1/s/tmp/a"},
		{"/tmp/cache/x", "/u/u1/s/cache/x"},
		{"/other", "/other"},
	} {
		if got := ApplyAlias(tc.in, aliases); got != tc.want {
			t.Errorf("ApplyAlias(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCloudPathPolicyResolve(t *testing.T) {
	policy := CloudPathPolicy{
		Root:    "/u/u1",
		Aliases: map[string]string{"/tmp": "/u/u1/.sessions/s1/tmp"},
		Jail:    []string{"/skills", "/public"},
	}
	for _, tc := range []struct{ in, want string }{
		{"notes.txt", "/u/u1/notes.txt"},
		{"/tmp/x", "/u/u1/.sessions/s1/tmp/x"},
		{"/skills/foo", "/skills/foo"},
		{"/public/bar", "/public/bar"},
	} {
		got, err := policy.Resolve(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{"../etc/passwd", "/tmp/../../etc/passwd", "/etc/passwd"} {
		got, err := policy.Resolve(in)
		if err == nil || got != "" || !strings.Contains(err.Error(), "jail") {
			t.Errorf("Resolve(%q) = %q, %v; want jail error", in, got, err)
		}
	}
}
