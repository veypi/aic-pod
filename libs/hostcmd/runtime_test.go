package hostcmd

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/veypi/aic-pod/protocol/hosts"
)

func fixture(t *testing.T, run func(context.Context, Call) (any, error)) (*Runtime, Caller, Session) {
	t.Helper()
	r, err := New(Config{Authorize: func(context.Context, Call) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	p := Provider{Descriptor: hosts.Command{Name: "test", Contract: "test/1", Methods: map[string]hosts.Method{"write": {InputSchema: json.RawMessage(`{"type":"object"}`), Effect: "write"}}}, Validate: func(_ string, args json.RawMessage) error {
		var p struct {
			Value int `json:"value"`
		}
		return hosts.Decode(args, &p)
	}, Scope: func(Call) string { return "device-input" }, Run: run}
	if err = r.Register(p); err != nil {
		t.Fatal(err)
	}
	c := Caller{Subject: "owner", ConnectionID: "conn_1", ExpiresAt: time.Now().Add(time.Hour)}
	s, err := r.Open(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := r.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return r, c, s
}
func invocation(s Session, id, args string) hosts.Invocation {
	return hosts.Invocation{SessionID: s.ID, RuntimeEpoch: s.RuntimeEpoch, OperationID: id, Command: "test", Method: "write", Args: json.RawMessage(args)}
}
func result(t *testing.T, r *Runtime, c Caller, s Session, id string) hosts.Operation {
	t.Helper()
	o, err := r.Get(context.Background(), c, s.ID, id, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !o.Terminal() {
		t.Fatalf("not finished: %+v", o)
	}
	return o
}
func TestConcurrentDuplicateAndConflict(t *testing.T) {
	var calls atomic.Int32
	r, c, s := fixture(t, func(context.Context, Call) (any, error) { calls.Add(1); return map[string]int{"value": 5}, nil })
	var wg sync.WaitGroup
	for range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.Invoke(context.Background(), c, invocation(s, "op_1", `{"value":5}`)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if o := result(t, r, c, s, "op_1"); o.Status != "succeeded" {
		t.Fatalf("%+v", o)
	}
	if calls.Load() != 1 {
		t.Fatalf("executed %d times", calls.Load())
	}
	if _, err := r.Invoke(context.Background(), c, invocation(s, "op_1", `{"value":6}`)); err == nil {
		t.Fatal("changed operation accepted")
	}
}
func TestDisconnectResumeNoReplay(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	r, c, s := fixture(t, func(ctx context.Context, _ Call) (any, error) { close(started); <-release; return "done", nil })
	if _, err := r.Invoke(context.Background(), c, invocation(s, "op_1", `{}`)); err != nil {
		t.Fatal(err)
	}
	<-started
	r.Disconnect(c.ConnectionID)
	next := c
	next.ConnectionID = "conn_2"
	if _, err := r.Get(context.Background(), next, s.ID, "op_1", 0); err == nil {
		t.Fatal("unbound connection accessed session")
	}
	if _, err := r.Resume(next, s.ID, s.RuntimeEpoch, "wrong"); err == nil {
		t.Fatal("bad token accepted")
	}
	if _, err := r.Resume(next, s.ID, s.RuntimeEpoch, s.ResumeToken); err != nil {
		t.Fatal(err)
	}
	close(release)
	if o := result(t, r, next, s, "op_1"); o.Status != "succeeded" {
		t.Fatalf("%+v", o)
	}
	if _, err := r.Invoke(context.Background(), next, invocation(s, "op_1", `{}`)); err != nil {
		t.Fatal(err)
	}
}
func TestCancelDoesNotReleaseRunningResource(t *testing.T) {
	started, release, second := make(chan struct{}), make(chan struct{}), make(chan struct{})
	r, c, s := fixture(t, func(ctx context.Context, call Call) (any, error) {
		if call.OperationID == "op_1" {
			close(started)
			<-ctx.Done()
			<-release
			return nil, ctx.Err()
		}
		close(second)
		return "second", nil
	})
	_, err := r.Invoke(context.Background(), c, invocation(s, "op_1", `{}`))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err = r.Cancel(c, s.ID, "op_1"); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Invoke(context.Background(), c, invocation(s, "op_2", `{}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-second:
		t.Fatal("cancel released scope before handler stopped")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if o := result(t, r, c, s, "op_1"); o.Status != "cancelled" || o.Error.Effect != "unknown" {
		t.Fatalf("%+v", o)
	}
	if o := result(t, r, c, s, "op_2"); o.Status != "succeeded" {
		t.Fatalf("%+v", o)
	}
}
func TestDeniedUnregisteredAndBadArgsNeverRun(t *testing.T) {
	r, c, s := fixture(t, func(context.Context, Call) (any, error) { t.Error("unexpected execution"); return nil, nil })
	bad := []hosts.Invocation{invocation(s, "op_1", `{"granted_level":9}`), invocation(s, "op_2", `null`)}
	unknown := invocation(s, "op_3", `{}`)
	unknown.Command = "undeclared"
	bad = append(bad, unknown)
	stale := invocation(s, "op_4", `{}`)
	stale.RuntimeEpoch = "old_epoch"
	bad = append(bad, stale)
	for _, in := range bad {
		if _, err := r.Invoke(context.Background(), c, in); err == nil {
			t.Fatal("accepted bad invocation")
		}
	}
	intruder := c
	intruder.Subject = "another-user"
	if _, err := r.Invoke(context.Background(), intruder, invocation(s, "op_5", `{}`)); err == nil {
		t.Fatal("cross-user access")
	}
	expired := c
	expired.ExpiresAt = time.Now().Add(-time.Second)
	if _, err := r.Invoke(context.Background(), expired, invocation(s, "op_6", `{}`)); err == nil {
		t.Fatal("expired authorization")
	}
}

func TestCatalogIsDetachedFromProviderAndConsumer(t *testing.T) {
	r, c, _ := fixture(t, func(context.Context, Call) (any, error) { return nil, nil })
	p := Provider{Descriptor: hosts.Command{Name: "another", Contract: "test/1", Methods: map[string]hosts.Method{"read": {InputSchema: json.RawMessage(`{"type":"object"}`), Effect: "read"}}}, Validate: func(string, json.RawMessage) error { return nil }, Run: func(context.Context, Call) (any, error) { return nil, nil }}
	if err := r.Register(p); err != nil {
		t.Fatal(err)
	}
	delete(p.Descriptor.Methods, "read")
	_, catalog, err := r.Catalog(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := catalog[0].Methods["read"]; !ok {
		t.Fatal("provider mutation changed catalog")
	}
	delete(catalog[0].Methods, "read")
	_, again, err := r.Catalog(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := again[0].Methods["read"]; !ok {
		t.Fatal("consumer mutation changed catalog")
	}
}

func TestOperationWaitRechecksAuthorization(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	r, c, s := fixture(t, func(context.Context, Call) (any, error) { close(started); <-release; return "secret", nil })
	var denied atomic.Bool
	var watch atomic.Bool
	waiting := make(chan struct{}, 1)
	r.cfg.Authorize = func(context.Context, Call) error {
		if denied.Load() {
			return hosts.Fail("permission_denied", "revoked")
		}
		if watch.Load() {
			select {
			case waiting <- struct{}{}:
			default:
			}
		}
		return nil
	}
	if _, err := r.Invoke(context.Background(), c, invocation(s, "op_1", `{}`)); err != nil {
		t.Fatal(err)
	}
	<-started
	watch.Store(true)
	done := make(chan error, 1)
	go func() { _, err := r.Get(context.Background(), c, s.ID, "op_1", time.Second); done <- err }()
	<-waiting
	denied.Store(true)
	close(release)
	if err := <-done; err == nil {
		t.Fatal("wait returned a result after authorization was revoked")
	}
}

func TestQueuedAdmissionSurvivesDisconnectAndRechecksPolicy(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	r, c, s := fixture(t, func(ctx context.Context, call Call) (any, error) {
		calls.Add(1)
		if call.OperationID == "first" {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return "done", nil
	})
	for _, id := range []string{"first", "second"} {
		if _, err := r.Invoke(context.Background(), c, invocation(s, id, `{}`)); err != nil {
			t.Fatal(err)
		}
		if id == "first" {
			<-entered
		}
	}
	r.Disconnect(c.ConnectionID)
	close(release)
	// No frontend is needed while the second admitted command drains the queue.
	next := c
	next.ConnectionID = "next"
	if _, err := r.Resume(next, s.ID, s.RuntimeEpoch, s.ResumeToken); err != nil {
		t.Fatal(err)
	}
	if op := result(t, r, next, s, "second"); op.Status != "succeeded" {
		t.Fatal(op)
	}
	if calls.Load() != 2 {
		t.Fatal("admission was lost", calls.Load())
	}
}

func TestScopedSessionCannotWidenOrReusePreviousRecoveryToken(t *testing.T) {
	r, c, _ := fixture(t, func(context.Context, Call) (any, error) { return true, nil })
	s, err := r.OpenScoped(c, []string{"fs"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Invoke(context.Background(), c, invocation(s, "not_fs", `{}`)); err == nil || hosts.AsFault(err).Code != "permission_denied" {
		t.Fatal("session scope bypass", err)
	}
	next := c
	next.ConnectionID = "proxy"
	next.Scopes = []string{"fs"}
	resumed, err := r.ResumeScoped(next, s.ID, s.RuntimeEpoch, s.ResumeToken, true)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := r.ResumeScoped(next, s.ID, s.RuntimeEpoch, s.ResumeToken, true)
	if err != nil || retry.ResumeToken != resumed.ResumeToken {
		t.Fatal("resume response retry", err)
	}
	if _, err = r.ResumeScoped(c, s.ID, s.RuntimeEpoch, s.ResumeToken, true); err == nil {
		t.Fatal("stale recovery token took back session")
	}
}
