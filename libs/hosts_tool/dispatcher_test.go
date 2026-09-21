package hosts_tool

import (
	"context"
	"encoding/json"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

type testArgs struct {
	Value string `json:"value" required:"true"`
}
type testStream struct{ closed atomic.Bool }

func (s *testStream) Recv(ctx context.Context) ([]byte, error) {
	if s.closed.Load() {
		return nil, io.EOF
	}
	return []byte("test"), nil
}
func (s *testStream) Send(context.Context, []byte) error { return nil }
func (s *testStream) Close() error                       { s.closed.Store(true); return nil }
func testCaller() Caller {
	return Caller{Subject: "owner", ConnectionID: "a", Level: 2, ExpiresAt: time.Now().Add(time.Minute)}
}
func TestCallsAndStreamsHaveNoBusinessSession(t *testing.T) {
	ctx := context.Background()
	d := New(Config{})
	var count atomic.Int64
	source := &testStream{}
	err := d.RegisterCommand(DefineCommand("fixture", Bind(Spec{Name: "echo", Access: 1, CLI: "echo", Positionals: []string{"value"}}, func(ctx context.Context, c Caller, a testArgs) (string, error) { count.Add(1); return a.Value, nil }), BindStream(Spec{Name: "watch", Access: 2}, func(context.Context, Caller, testArgs) (Stream, error) { return source, nil })))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close(ctx)
	c := testCaller()
	call := wire.Request{ID: "r1", Action: "call", Argv: []string{"fixture", "echo", "hello"}}
	r := d.Handle(ctx, c, call)
	if r.Error != nil || r.Result != "hello" {
		t.Fatal(r)
	}
	r = d.Handle(ctx, c, call)
	if r.Error != nil || count.Load() != 2 {
		t.Fatal("request_id became a retained operation")
	}
	call.Argv = nil
	call.Call = &wire.Invocation{Domain: "exec", Command: "fixture", Method: "echo", Args: json.RawMessage(`{"value":"x","unknown":1}`)}
	if d.Handle(ctx, c, call).Error == nil {
		t.Fatal("unknown field accepted")
	}
	call.Call.Args = json.RawMessage(`{}`)
	if d.Handle(ctx, c, call).Error == nil {
		t.Fatal("required field accepted")
	}
	inv := wire.Invocation{Domain: "exec", Command: "fixture", Method: "watch", Args: json.RawMessage(`{"value":"x"}`)}
	if _, err := d.OpenStream(ctx, c, inv); err == nil {
		t.Fatal("non-RTC stream admitted")
	}
	if r := d.Handle(ctx, c, wire.Request{ID: "legacy", Action: "stream.open", Call: &inv}); r.Error == nil {
		t.Fatal("RPC stream action admitted")
	}
	c.AllowStreams = true
	stream, err := d.OpenStream(ctx, c, inv)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(ctx, []byte{0, 255, 1}); err != nil {
		t.Fatal(err)
	}
	b, err := stream.Recv(ctx)
	if err != nil || string(b) != "test" {
		t.Fatalf("%q %v", b, err)
	}
	d.Disconnect(c)
	if !source.closed.Load() {
		t.Fatal("stream not closed on disconnect")
	}
	if len(d.streams) != 0 || len(d.calls) != 0 {
		t.Fatal("transport state leaked")
	}
}
func TestCancellationAndAuthorization(t *testing.T) {
	d := New(Config{})
	defer d.Close(context.Background())
	started := make(chan struct{})
	d.RegisterCommand(DefineCommand("fixture", Bind(Spec{Name: "wait", Access: 2}, func(ctx context.Context, c Caller, a struct{}) (bool, error) {
		close(started)
		<-ctx.Done()
		return false, ctx.Err()
	})))
	c := testCaller()
	r := wire.Request{ID: "waiting", Action: "call", Call: &wire.Invocation{Domain: "exec", Command: "fixture", Method: "wait", Args: json.RawMessage(`{}`)}}
	weak := c
	weak.Level = 1
	if d.Handle(context.Background(), weak, r).Error == nil {
		t.Fatal("grant bypassed")
	}
	done := make(chan wire.Response, 1)
	go func() { done <- d.Handle(context.Background(), c, r) }()
	<-started
	cancel := d.Handle(context.Background(), c, wire.Request{ID: "cancel", Action: "call.cancel", CancelID: r.ID})
	if cancel.Error != nil {
		t.Fatal(cancel)
	}
	select {
	case result := <-done:
		if result.Error == nil || result.Error.Code != "cancelled" {
			t.Fatal(result)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel blocked by running call")
	}
}

func TestDeclaredArgumentGrantAndLiteralEqualsValue(t *testing.T) {
	type args struct {
		Value    string `json:"value"`
		Delivery string `json:"delivery,omitempty"`
	}
	d := New(Config{})
	defer d.Close(context.Background())
	method := Bind(Spec{Name: "write", Access: 2, AccessRules: []wire.AccessRule{{Field: "delivery", Equals: "foreground", Level: 3}}}, func(_ context.Context, _ Caller, a args) (string, error) { return a.Value, nil })
	if err := d.RegisterCommand(DefineCommand("fixture", method)); err != nil {
		t.Fatal(err)
	}
	call := wire.Request{ID: "inline", Action: "call", Argv: []string{"fixture", "write", "--value=keep-hyphens", "--delivery", "foreground"}}
	caller := testCaller()
	if r := d.Handle(context.Background(), caller, call); r.Error == nil || r.Error.Code != "permission_denied" {
		t.Fatalf("dynamic grant bypass: %+v", r)
	}
	caller.Level = 3
	if r := d.Handle(context.Background(), caller, call); r.Error != nil || r.Result != "keep-hyphens" {
		t.Fatalf("literal changed: %+v", r)
	}
}

func TestCloseStillTearsDownToolAfterCallDeadline(t *testing.T) {
	d := New(Config{})
	started, stop, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	binding := DefineCommand("fixture", Bind(Spec{Name: "wait", Access: 1}, func(context.Context, Caller, struct{}) (bool, error) { close(started); <-stop; return true, nil }))
	binding.Close = func() error { close(stop); return nil }
	if err := d.RegisterCommand(binding); err != nil {
		t.Fatal(err)
	}
	go func() {
		d.Handle(context.Background(), testCaller(), wire.Request{ID: "pending", Action: "call", Argv: []string{"fixture", "wait"}})
		close(finished)
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := d.Close(ctx); err == nil {
		t.Fatal("missed shutdown deadline not reported")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("tool teardown skipped")
	}
}

func TestStreamOpeningCancelledByDisconnect(t *testing.T) {
	d := New(Config{})
	defer d.Close(context.Background())
	started := make(chan struct{})
	source := &testStream{}
	if err := d.RegisterCommand(DefineCommand("fixture", BindStream(Spec{Name: "watch", Access: 1}, func(ctx context.Context, _ Caller, _ struct{}) (Stream, error) {
		close(started)
		<-ctx.Done()
		return source, nil
	}))); err != nil {
		t.Fatal(err)
	}
	c := testCaller()
	c.AllowStreams = true
	done := make(chan error, 1)
	go func() {
		_, err := d.OpenStream(context.Background(), c, wire.Invocation{Domain: "exec", Command: "fixture", Method: "watch", Args: json.RawMessage(`{}`)})
		done <- err
	}()
	<-started
	d.Disconnect(c)
	select {
	case err := <-done:
		if err == nil || !source.closed.Load() {
			t.Fatalf("late source retained: %v, closed=%v", err, source.closed.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("opening survived disconnect")
	}
	if len(d.streams) != 0 {
		t.Fatal("attachment leaked")
	}
}

func TestStreamOnlyInRTCCatalogAndUsesCurrentAuthorization(t *testing.T) {
	d := New(Config{})
	defer d.Close(context.Background())
	if err := d.RegisterCommand(DefineCommand("fixture", BindStream(Spec{Name: "watch", Access: 1}, func(ctx context.Context, _ Caller, _ struct{}) (Stream, error) {
		if _, fixed := ctx.Deadline(); fixed {
			t.Error("stream pinned to opening ticket deadline")
		}
		return &testStream{}, nil
	}))); err != nil {
		t.Fatal(err)
	}
	c := testCaller()
	if len(d.Commands(context.Background(), c)) != 0 {
		t.Fatal("stream exposed outside RTC")
	}
	c.AllowStreams = true
	c.ExpiresAt = time.Now().Add(-time.Minute)
	c.Expiry = func() time.Time { return time.Now().Add(time.Minute) }
	if len(d.Commands(context.Background(), c)) != 1 {
		t.Fatal("RTC stream missing")
	}
	s, err := d.OpenStream(context.Background(), c, wire.Invocation{Domain: "exec", Command: "fixture", Method: "watch", Args: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
}
