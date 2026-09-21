package pod

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/veypi/aic-pod/cfg"
)

func TestStartHandshake(t *testing.T) {
	previous := cfg.Global
	cfg.Global = cfg.NewOptions()
	t.Cleanup(func() { Stop(); cfg.Global = previous })
	portFile := filepath.Join(t.TempDir(), "port.json")
	t.Setenv("AIC_PORT_FILE", portFile)
	if err := Start(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(portFile)
	if err != nil {
		t.Fatal(err)
	}
	var info struct {
		Port int    `json:"port"`
		Code string `json:"code"`
	}
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatal(err)
	}
	if info.Port != cfg.Global.Port() || info.Port == 0 || info.Code == "" || info.Code != cfg.Global.Code {
		t.Fatal("invalid startup handshake")
	}
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/ping", info.Port))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("backend not ready: %d", resp.StatusCode)
	}
}

func TestStartFailsWhenHandshakeCannotBeWritten(t *testing.T) {
	previous := cfg.Global
	cfg.Global = cfg.NewOptions()
	t.Cleanup(func() { Stop(); cfg.Global = previous })
	t.Setenv("AIC_PORT_FILE", filepath.Join(t.TempDir(), "missing", "port.json"))
	if err := Start(); err == nil || !strings.Contains(err.Error(), "write port file") {
		t.Fatalf("expected handshake write error, got %v", err)
	}
	if cfg.Global.Port() != 0 || srv != nil {
		t.Fatal("failed startup retained a running server")
	}
}

func TestNextRetryDelay(t *testing.T) {
	cases := []struct {
		delay, max, want time.Duration
	}{
		{5 * time.Second, 5 * time.Minute, 10 * time.Second},
		{3 * time.Minute, 5 * time.Minute, 5 * time.Minute},
		{5 * time.Minute, 5 * time.Minute, 5 * time.Minute}, // 封顶后保持
		{1 * time.Second, 1 * time.Second, 1 * time.Second},
	}
	for i, c := range cases {
		if got := nextRetryDelay(c.delay, c.max); got != c.want {
			t.Errorf("case %d: nextRetryDelay(%v, %v) = %v, want %v", i, c.delay, c.max, got, c.want)
		}
	}
}

func TestRetryHostStartWith_FirstTrySuccess(t *testing.T) {
	calls := 0
	var sleeps []time.Duration
	start := func(cfg.Options) error {
		calls++
		return nil
	}
	sleep := func(d time.Duration) { sleeps = append(sleeps, d) }
	retryHostStartWith(&cfg.Options{}, start, sleep, time.Second, time.Minute)
	if calls != 1 {
		t.Fatalf("start called %d times, want 1", calls)
	}
	if len(sleeps) != 1 {
		t.Fatalf("sleep called %d times, want 1 (initial delay before first try)", len(sleeps))
	}
}

func TestRetryHostStartWith_Backoff(t *testing.T) {
	calls := 0
	var sleeps []time.Duration
	start := func(cfg.Options) error {
		calls++
		if calls < 4 {
			return fmt.Errorf("nats connect: nats: Authorization Violation")
		}
		return nil
	}
	retryHostStartWith(&cfg.Options{}, start, func(d time.Duration) { sleeps = append(sleeps, d) }, time.Second, time.Minute)
	if calls != 4 {
		t.Fatalf("start called %d times, want 4 (3 failures + 1 success)", calls)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	if len(sleeps) != len(want) {
		t.Fatalf("sleeps = %v, want %v", sleeps, want)
	}
	for i, d := range sleeps {
		if d != want[i] {
			t.Errorf("sleep[%d] = %v, want %v", i, d, want[i])
		}
	}
}

func TestRetryHostStartWith_PermanentAbort(t *testing.T) {
	for _, msg := range []string{"key is empty — bind a device first", "host already running"} {
		calls := 0
		start := func(cfg.Options) error {
			calls++
			return fmt.Errorf("%s", msg)
		}
		retryHostStartWith(&cfg.Options{}, start, func(time.Duration) {}, time.Second, time.Minute)
		if calls != 1 {
			t.Errorf("%q: start called %d times, want 1 (abort on permanent error)", msg, calls)
		}
	}
}

func TestIsPermanentStartErr(t *testing.T) {
	if !isPermanentStartErr(fmt.Errorf("key is empty — bind a device first")) {
		t.Error("key is empty should be permanent")
	}
	if !isPermanentStartErr(fmt.Errorf("invalid credential key")) {
		t.Error("invalid credential key should be permanent")
	}
	if !isPermanentStartErr(fmt.Errorf("invalid credential key version")) {
		t.Error("invalid credential key version should be permanent")
	}
	if !isPermanentStartErr(fmt.Errorf("host already running")) {
		t.Error("already running should be permanent")
	}
	if isPermanentStartErr(fmt.Errorf("nats connect: nats: Authorization Violation")) {
		t.Error("auth violation is transient and must retry")
	}
	if isPermanentStartErr(fmt.Errorf("connect: connection refused")) {
		t.Error("network error is transient and must retry")
	}
	if isPermanentStartErr(nil) {
		t.Error("nil is not an error")
	}
}
