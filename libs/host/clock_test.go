package host

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	tool "github.com/veypi/aic-pod/libs/hosts_tool"
	natswire "github.com/veypi/aic-pod/protocol/hosts_nats"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
)

func TestResolveTimeURL(t *testing.T) {
	cases := map[string]string{
		"https://ivec-ai.com":            "https://ivec-ai.com/api/time",
		"http://localhost:4000":          "http://localhost:4000/api/time",
		"http://127.0.0.1:4000/rses/aiv": "http://127.0.0.1:4000/rses/aiv/api/time",
		"ws://127.0.0.1:4002":            "http://127.0.0.1:4002/api/time",
		"wss://ivec-ai.com/":             "https://ivec-ai.com/api/time",
		"ivec-ai.com":                    "https://ivec-ai.com/api/time",
	}
	for in, want := range cases {
		if got := resolveTimeURL(in); got != want {
			t.Errorf("resolveTimeURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClockOffsetFromCompensatesRoundTrip(t *testing.T) {
	t0 := time.UnixMilli(1_000_000)
	t1 := t0.Add(200 * time.Millisecond)
	// 服务端在收到请求后 100ms（往返中点）发出时间戳，比本机快 5s。
	serverMS := t0.Add(100*time.Millisecond).UnixMilli() + 5_000
	if got := clockOffsetFrom(serverMS, t0, t1); got != 5_000 {
		t.Fatalf("offset = %d, want 5000", got)
	}
}

func TestFetchServerTime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/time" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"now":1800000000000}`))
	}))
	defer srv.Close()

	got, err := fetchServerTime(srv.URL)
	if err != nil || got != 1_800_000_000_000 {
		t.Fatalf("got %d, err %v", got, err)
	}
	// 带路径前缀时拼接为 /<prefix>/api/time；mock 只认 /api/time → 404 → 必须报错。
	if _, err := fetchServerTime(srv.URL + "/prefixed/"); err == nil {
		t.Fatal("expected error for non-200 response")
	}
}

func TestClockNowAppliesOffset(t *testing.T) {
	defer setClockOffset(0)
	before := time.Now()
	setClockOffset((90 * time.Minute).Milliseconds())
	if d := clockNow().Sub(before.Add(90 * time.Minute)); d < -2*time.Second || d > 2*time.Second {
		t.Fatalf("clockNow deviation too large: %v", d)
	}
	// 零偏移退化为系统时间（未对时/旧平台行为）。
	setClockOffset(0)
	if d := clockNow().Sub(time.Now()); d < -2*time.Second || d > 2*time.Second {
		t.Fatalf("zero offset should follow system time: %v", d)
	}
}

// TestToolsVerifyUsesCalibratedClock 锁定跨机时效校验使用校准时钟：
// 设备本机时钟快 2 小时（模拟系统时间误差），平台请求时限按平台时间签署，
// 校准后必须通过；仍按本机系统时间视角签署的时限必须被拒。
func TestToolsVerifyUsesCalibratedClock(t *testing.T) {
	c := New(Options{Key: "host_1.1.secret.owner", WorkDir: t.TempDir(), BrowserStateDir: t.TempDir()})
	t.Cleanup(func() { _ = c.Close() })
	c.hostID, c.uid, c.kTool = "host_1", "owner", "test-tool-key"
	d := tool.New(tool.Config{})
	if err := d.RegisterCommand(tool.DefineCommand("counter", tool.Bind(tool.Spec{Name: "next", Access: 2}, func(ctx context.Context, caller tool.Caller, _ struct{}) (int, error) {
		return 1, nil
	}))); err != nil {
		t.Fatal(err)
	}
	original := c.tools
	c.tools = d
	t.Cleanup(func() { _ = original.Close(context.Background()) })
	defer setClockOffset(0)

	route, _ := natswire.Subject(c.uid, c.hostID)
	setClockOffset((2 * time.Hour).Milliseconds())
	makeReq := func(deadlineMS, untilMS int64) []byte {
		r := natswire.Request{HostID: c.hostID, Subject: route, Caller: c.uid, GrantedLevel: 2, Nonce: wire.NewID("n_"), Deadline: deadlineMS, AuthorizationUntil: untilMS, Request: wire.Request{Protocol: natswire.Protocol, ID: wire.NewID("r_"), Action: "call", Argv: []string{"counter", "next"}}}
		natswire.Sign(c.kTool, &r)
		b, _ := json.Marshal(r)
		return b
	}
	// 平台时间视角的时限（校准后 now 对应平台时间）。
	if got := c.HandleNATS(context.Background(), route, makeReq(clockNow().Add(time.Minute).UnixMilli(), clockNow().Add(2*time.Minute).UnixMilli())); got.Error != nil {
		t.Fatalf("calibrated window rejected: %+v", got)
	}
	// 本机系统时间视角的时限（比平台时间早 2 小时）必须判失效。
	if got := c.HandleNATS(context.Background(), route, makeReq(time.Now().Add(time.Minute).UnixMilli(), time.Now().Add(2*time.Minute).UnixMilli())); got.Error == nil {
		t.Fatal("system-time based window admitted")
	}
}
