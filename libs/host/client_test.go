package host

import (
	"errors"
	"testing"
)

// isAuthError 须匹配 nats.go 的大写错误（"Authentication Violation"/
// "Authorization Violation"），否则致命认证分支永不触发、无限静默重连。
func TestIsAuthError(t *testing.T) {
	yes := []error{
		errors.New(`nats: Authorization Violation - User "u1"`),
		errors.New("nats: Authentication Violation"),
		errors.New("authorization violation"),
	}
	for _, err := range yes {
		if !isAuthError(err) {
			t.Errorf("isAuthError(%q) = false, want true", err)
		}
	}
	no := []error{
		errors.New("nats: timeout"),
		errors.New("read: connection reset by peer"),
	}
	for _, err := range no {
		if isAuthError(err) {
			t.Errorf("isAuthError(%q) = true, want false", err)
		}
	}
}


// 正常关闭（nc.Close()）时 DisconnectErrHandler 的 err 为 nil，不得 panic。
func TestIsAuthErrorNil(t *testing.T) {
	if isAuthError(nil) {
		t.Error("isAuthError(nil) = true, want false")
	}
}
