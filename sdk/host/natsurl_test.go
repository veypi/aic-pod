package host

import "testing"

func TestResolveNATSURL(t *testing.T) {
	cases := []struct {
		name string
		host string
		url  string
		want string
	}{
		{"default-host-https", "", "", "wss://ivec.ai/aic/api/nc"},
		{"https-host", "https://ivec.ai", "", "wss://ivec.ai/aic/api/nc"},
		{"http-host", "http://localhost:4000", "", "ws://localhost:4000/aic/api/nc"},
		{"no-scheme", "ivec.ai", "", "wss://ivec.ai/aic/api/nc"},
		{"ws-scheme-kept", "ws://localhost:4000", "", "ws://localhost:4000/aic/api/nc"},
		{"host-with-path-ignored", "https://ivec.ai/console", "", "wss://ivec.ai/aic/api/nc"},
		{"host-with-port", "https://localhost:4000", "", "wss://localhost:4000/aic/api/nc"},
		{"explicit-url-wins", "http://x", "wss://ivec.ai/aic/api/nc", "wss://ivec.ai/aic/api/nc"},
		{"explicit-url-empty-host", "", "nats://127.0.0.1:4222", "nats://127.0.0.1:4222"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ResolveNATSURL(c.host, c.url); got != c.want {
				t.Fatalf("ResolveNATSURL(%q, %q) = %q, want %q", c.host, c.url, got, c.want)
			}
		})
	}
}
