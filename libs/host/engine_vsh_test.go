package host

import (
	"net/url"
	"testing"
)

func TestIsConfiguredPlaintextPlatform(t *testing.T) {
	cases := []struct {
		name   string
		target string
		host   string
		want   bool
	}{
		{name: "配置的平台 http 地址放行", target: "http://host.docker.internal:4002/skills/cloud/x/api/scan", host: "http://host.docker.internal:4002", want: true},
		{name: "配置即信任的内网域名", target: "http://self.domain/api/x", host: "http://self.domain", want: true},
		{name: "省略端口与显式 80 等价", target: "http://self.domain:80/api/x", host: "http://self.domain", want: true},
		{name: "主机名大小写不敏感", target: "http://Self.Domain/api/x", host: "http://self.domain:80", want: true},
		{name: "第三方 http 地址仍拒绝", target: "http://evil.example.com/skills/cloud/x/api/scan", host: "http://host.docker.internal:4002", want: false},
		{name: "端口不同不是同一端点", target: "http://self.domain:9999/api/x", host: "http://self.domain:4002", want: false},
		{name: "配置为 https 时请求 http 不放行", target: "http://self.domain/api/x", host: "https://self.domain", want: false},
		{name: "https 请求无需此豁免", target: "https://self.domain/api/x", host: "http://self.domain", want: false},
		{name: "空配置不放行", target: "http://self.domain/api/x", host: "", want: false},
		{name: "非法配置不放行", target: "http://self.domain/api/x", host: "not a url", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.target)
			if err != nil {
				t.Fatalf("parse target: %v", err)
			}
			if got := isConfiguredPlaintextPlatform(u, tc.host); got != tc.want {
				t.Fatalf("isConfiguredPlaintextPlatform(%q, %q) = %v, want %v", tc.target, tc.host, got, tc.want)
			}
		})
	}
}
