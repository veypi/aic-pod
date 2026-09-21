package host

import (
	"testing"
)

func TestParseWSURL(t *testing.T) {
	cases := []struct {
		raw       string
		base      string
		proxyPath string
	}{
		{"wss://ivec-ai.com/api/nc", "wss://ivec-ai.com", "/api/nc"},
		{"ws://host/path", "ws://host", "/path"},
		{"wss://host", "wss://host", ""},
		{"ws://host:8080/a/b", "ws://host:8080", "/a/b"},
		{"ws://host/", "ws://host/", ""},
	}
	for _, c := range cases {
		u, err := parseWSURL(c.raw)
		if err != nil {
			t.Errorf("%s: unexpected err %v", c.raw, err)
			continue
		}
		if u.base != c.base || u.proxyPath != c.proxyPath {
			t.Errorf("%s: got base=%q proxy=%q, want base=%q proxy=%q",
				c.raw, u.base, u.proxyPath, c.base, c.proxyPath)
		}
	}
}

// 本地命令 workdir 缺省回落：请求未携带 workdir 时使用 host 端配置工作区（§2.1.1）。
