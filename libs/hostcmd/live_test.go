package hostcmd

import (
	"context"
	"encoding/json"
	"github.com/veypi/aic-pod/protocol/hosts"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testLive struct {
	done  chan struct{}
	once  sync.Once
	sends atomic.Int32
}

func (l *testLive) Recv() (LiveItem, error)    { <-l.done; return LiveItem{}, context.Canceled }
func (l *testLive) Send(json.RawMessage) error { l.sends.Add(1); return nil }
func (l *testLive) Close() error               { l.once.Do(func() { close(l.done) }); return nil }
func registerLive(t *testing.T, r *Runtime, open func(context.Context, Call) (Live, error)) {
	t.Helper()
	err := r.Register(Provider{Descriptor: hosts.Command{Name: "test", Contract: "test/1", Methods: map[string]hosts.Method{
		"view": {Mode: "live", Effect: "write", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}}, Validate: func(string, json.RawMessage) error { return nil }, Run: func(context.Context, Call) (any, error) { t.Error("Live method invoked as a request"); return nil, nil }, OpenLive: open})
	if err != nil {
		t.Fatal(err)
	}
}
func TestLiveSessionIsolationRevocationAndCleanup(t *testing.T) {
	r, c, s := fixture(t, func(context.Context, Call) (any, error) { return nil, nil })
	var denied atomic.Bool
	r.cfg.Authorize = func(context.Context, Call) error {
		if denied.Load() {
			return hosts.Fail("unauthorized", "Revoked")
		}
		return nil
	}
	source := &testLive{done: make(chan struct{})}
	registerLive(t, r, func(context.Context, Call) (Live, error) { return source, nil })
	args := RequestCall{SessionID: s.ID, RuntimeEpoch: s.RuntimeEpoch, Command: "test", Method: "view", Args: json.RawMessage(`{}`)}
	stranger := c
	stranger.ConnectionID = "other"
	if _, err := r.OpenLive(context.Background(), stranger, args); err == nil {
		t.Fatal("Unbound caller opened a stream")
	}
	if _, err := r.Request(context.Background(), c, args); err == nil {
		t.Fatal("Live method accepted as request")
	}
	op := invocation(s, "wrong_mode", `{}`)
	op.Method = "view"
	if _, err := r.Invoke(context.Background(), c, op); err == nil {
		t.Fatal("Live method accepted as invoke")
	}
	stream, err := r.OpenLive(context.Background(), c, args)
	if err != nil {
		t.Fatal(err)
	}
	if err = stream.Send(json.RawMessage(`{"events":[]}`)); err != nil {
		t.Fatal(err)
	}
	denied.Store(true)
	if err = stream.Send(json.RawMessage(`{}`)); err == nil {
		t.Fatal("Revoked caller sent input")
	}
	if source.sends.Load() != 1 {
		t.Fatal("Revoked input reached provider")
	}
	denied.Store(false)
	if err = r.CloseSession(stranger, s.ID); err == nil {
		t.Fatal("Foreign session close succeeded")
	}
	select {
	case <-source.done:
		t.Fatal("Foreign close stopped stream")
	default:
	}
	returned := make(chan struct{})
	go func() { stream.Recv(); close(returned) }()
	if err = r.CloseSession(c, s.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("Session close left blocked reader")
	}
	if err = stream.Send(json.RawMessage(`{}`)); err == nil {
		t.Fatal("Closed stream accepted input")
	}
}
func TestLiveLateOpenAndDisconnect(t *testing.T) {
	r, c, s := fixture(t, func(context.Context, Call) (any, error) { return nil, nil })
	started, release := make(chan struct{}), make(chan struct{})
	source := &testLive{done: make(chan struct{})}
	registerLive(t, r, func(context.Context, Call) (Live, error) { close(started); <-release; return source, nil })
	result := make(chan error, 1)
	go func() {
		_, err := r.OpenLive(context.Background(), c, RequestCall{SessionID: s.ID, RuntimeEpoch: s.RuntimeEpoch, Command: "test", Method: "view", Args: json.RawMessage(`{}`)})
		result <- err
	}()
	<-started
	r.Disconnect(c.ConnectionID)
	close(release)
	if err := <-result; err == nil {
		t.Fatal("Late disconnected open succeeded")
	}
	select {
	case <-source.done:
	default:
		t.Fatal("Late source leaked")
	}
}
func TestLiveProviderPanicReleasesAdmission(t *testing.T) {
	r, c, s := fixture(t, func(context.Context, Call) (any, error) { return nil, nil })
	registerLive(t, r, func(context.Context, Call) (Live, error) { panic("test") })
	_, err := r.OpenLive(context.Background(), c, RequestCall{SessionID: s.ID, RuntimeEpoch: s.RuntimeEpoch, Command: "test", Method: "view", Args: json.RawMessage(`{}`)})
	if err == nil || hosts.AsFault(err).Code != "internal" {
		t.Fatalf("panic: %v", err)
	}
	if err = r.CloseSession(c, s.ID); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	remaining := len(r.sessions)
	r.mu.Unlock()
	if remaining != 0 {
		t.Fatal("Panic leaked an active session")
	}
}
