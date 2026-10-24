package execution

import (
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
