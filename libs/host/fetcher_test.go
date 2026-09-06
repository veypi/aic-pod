package host

import (
	"strings"
	"testing"

	"github.com/veypi/aic-pod/libs/vcore"
)

// buildCurlArgv：方法/头/body 形态正确，-q/-sS/-L 恒在，URL 收尾。
func TestBuildCurlArgv(t *testing.T) {
	argv := buildCurlArgv(vcore.HTTPReq{Method: "GET", URL: "https://example.com/x"}, "example.com", 443, nil)
	want := "curl -q -sS -L --proto-redir =http,https -X GET https://example.com/x"
	if strings.Join(argv, " ") != want {
		t.Fatalf("argv = %v, want %q", argv, want)
	}

	argv = buildCurlArgv(vcore.HTTPReq{
		Method:  "POST",
		URL:     "http://localhost:8080/api",
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    []byte(`{"a":1}`),
	}, "localhost", 8080, nil)
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "-H Content-Type: application/json") ||
		!strings.Contains(joined, "--data-binary @-") ||
		!strings.HasSuffix(joined, "http://localhost:8080/api") {
		t.Fatalf("argv = %v", argv)
	}

	// --resolve 钉住（deny 锁定模式补偿沙箱内无 DNS）
	argv = buildCurlArgv(vcore.HTTPReq{Method: "GET", URL: "https://example.com/"}, "example.com", 443,
		[]string{"93.184.215.14", "2606:2800:21f:cb07:6820:80da:af6b:8b2c"})
	joined = strings.Join(argv, " ")
	if !strings.Contains(joined, "--resolve example.com:443:93.184.215.14") ||
		!strings.Contains(joined, "--resolve example.com:443:2606:2800:21f:cb07:6820:80da:af6b:8b2c") {
		t.Fatalf("argv missing --resolve pins: %v", argv)
	}
}
