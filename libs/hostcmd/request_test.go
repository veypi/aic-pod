package hostcmd

import (
	"context"
	"encoding/json"
	"github.com/veypi/aic-pod/protocol/hosts"
	"testing"
)

func TestTransientRequestsBoundedAndAuthorized(t *testing.T) {
	calls := 0
	r, c, s := fixture(t, func(context.Context, Call) (any, error) { return nil, nil })
	p := Provider{Descriptor: hosts.Command{Name: "live", Contract: "live/1", Methods: map[string]hosts.Method{"frame": {Mode: "request", Effect: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}}}, Validate: func(_ string, args json.RawMessage) error { var a struct{}; return hosts.Decode(args, &a) }, Run: func(context.Context, Call) (any, error) { calls++; return map[string]int{"n": calls}, nil }}
	if err := r.Register(p); err != nil {
		t.Fatal(err)
	}
	req := RequestCall{SessionID: s.ID, RuntimeEpoch: s.RuntimeEpoch, Command: "live", Method: "frame", Args: json.RawMessage(`{}`)}
	for range 5000 {
		if _, err := r.Request(context.Background(), c, req); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.sessions[s.ID].operations) != 0 {
		t.Fatal("observations retained as durable operations")
	}
	foreign := c
	foreign.ConnectionID = "other"
	if _, err := r.Request(context.Background(), foreign, req); err == nil {
		t.Fatal("foreign session accepted")
	}
	wrong := req
	wrong.Command = "test"
	wrong.Method = "write"
	wrong.Args = json.RawMessage(`{"value":1}`)
	if _, err := r.Request(context.Background(), c, wrong); err == nil {
		t.Fatal("durable write accepted as transient request")
	}
	invoke := hosts.Invocation{SessionID: s.ID, RuntimeEpoch: s.RuntimeEpoch, OperationID: "op", Command: "live", Method: "frame", Args: json.RawMessage(`{}`)}
	if _, err := r.Invoke(context.Background(), c, invoke); err == nil {
		t.Fatal("transient method admitted into ledger")
	}
	r.cfg.Authorize = func(context.Context, Call) error { return hosts.Fail("permission_denied", "revoked") }
	if _, err := r.Request(context.Background(), c, req); err == nil {
		t.Fatal("revoked request accepted")
	}
	if calls != 5000 {
		t.Fatalf("unexpected executions: %d", calls)
	}
}
