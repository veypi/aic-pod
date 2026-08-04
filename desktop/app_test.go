package main

import "testing"

func TestHostsURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "https://ivec.ai/hosts"},
		{"https://ivec.ai", "https://ivec.ai/hosts"},
		{"http://localhost:4000", "http://localhost:4000/hosts"},
		{"http://localhost:4000/", "http://localhost:4000/hosts"},
		{"http://127.0.0.1:4000/rses/aiv", "http://127.0.0.1:4000/rses/aiv/hosts"},
		{"https://ivec.ai/hosts", "https://ivec.ai/hosts"},
		{"ivec.ai", "https://ivec.ai/hosts"},
		{"http://x:1/?q=1", "http://x:1/hosts"},
	}
	for _, c := range cases {
		if got := hostsURL(c.in); got != c.want {
			t.Errorf("hostsURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
