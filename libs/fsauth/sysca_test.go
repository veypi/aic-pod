//
// sysca_test.go
// Copyright (C) 2026 veypi <i@veypi.com>
// Distributed under terms of the MIT license.
//

package fsauth

import (
	"runtime"
	"strings"
	"testing"
)

// SystemCAReadPatterns：平台清单非空（windows 除外）、含系统 CA 路径两形态
// （canonical + 字面）、无畸形模式。
func TestSystemCAReadPatterns(t *testing.T) {
	pats := SystemCAReadPatterns()
	switch runtime.GOOS {
	case "darwin":
		if len(pats) == 0 {
			t.Fatal("darwin: empty CA read patterns")
		}
		if !containsPattern(pats, "/etc/ssl/cert.pem") {
			t.Fatalf("darwin: missing /etc/ssl/cert.pem literal form: %v", pats)
		}
		if !containsPattern(pats, "/private/etc/ssl/cert.pem") {
			t.Fatalf("darwin: missing canonical form (/etc → /private/etc): %v", pats)
		}
	case "linux":
		if !containsPattern(pats, "/etc/ssl/certs/**") {
			t.Fatalf("linux: missing /etc/ssl/certs/**: %v", pats)
		}
	default:
		if pats != nil {
			t.Fatalf("unsupported platform should return nil: %v", pats)
		}
	}
	for _, p := range pats {
		if strings.Contains(p, "//") {
			t.Fatalf("pattern contains double slash: %q", p)
		}
		if strings.TrimSpace(p) == "" {
			t.Fatalf("empty pattern in %v", pats)
		}
	}
}

func containsPattern(pats []string, want string) bool {
	for _, p := range pats {
		if p == want {
			return true
		}
	}
	return false
}
