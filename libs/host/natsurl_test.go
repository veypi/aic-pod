package host

import "testing"

func TestResolveNATSURL(t *testing.T) {
	cases := []struct {
		name string
		host string
		want string
	}{
		{"default-host", "", "wss://ivec.ai/api/nc"},
		{"https-host", "https://ivec.ai", "wss://ivec.ai/api/nc"},
		{"http-host", "http://localhost:4000", "ws://localhost:4000/api/nc"},
		{"no-scheme", "ivec.ai", "wss://ivec.ai/api/nc"},
		{"ws-scheme-kept", "ws://localhost:4000", "ws://localhost:4000/api/nc"},
		{"host-with-port", "https://localhost:4000", "wss://localhost:4000/api/nc"},
		{"path-prefix", "http://127.0.0.1:4000/rses/aiv", "ws://127.0.0.1:4000/rses/aiv/api/nc"},
		{"path-prefix-trailing-slash", "https://ivec.ai/rses/aiv/", "wss://ivec.ai/rses/aiv/api/nc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ResolveNATSURL(c.host); got != c.want {
				t.Fatalf("ResolveNATSURL(%q) = %q, want %q", c.host, got, c.want)
			}
		})
	}
}
