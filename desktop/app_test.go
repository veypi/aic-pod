package main

import "testing"

func TestConsoleURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "https://ivec.ai/aic/"},
		{"https://ivec.ai", "https://ivec.ai/aic/"},
		{"http://127.0.0.1:4000", "http://127.0.0.1:4000/aic/"},
		{"http://127.0.0.1:4000/rses/aiv", "http://127.0.0.1:4000/rses/aiv/aic/"},
		{"https://ivec.ai/aic", "https://ivec.ai/aic/"},
		{"https://ivec.ai/aic/", "https://ivec.ai/aic/"},
		{"ivec.ai", "https://ivec.ai/aic/"},
		{"http://127.0.0.1:4000/?x=1", "http://127.0.0.1:4000/aic/"},
	}
	for _, c := range cases {
		if got := consoleURL(c.in); got != c.want {
			t.Errorf("consoleURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
