package hostcmd

import (
	"context"
	"encoding/json"
	"time"

	"github.com/veypi/aic-pod/protocol/hosts"
)

// Handle serves the authenticated control plane; transports perform hello and
// data binding first. It shares Runtime admission with in-process consumers.
func (r *Runtime) Handle(ctx context.Context, c Caller, raw []byte) hosts.Response {
	req, err := hosts.ParseRequest(raw)
	if err != nil {
		return hosts.Reply(req.ID, nil, err)
	}
	value, err := r.handle(ctx, c, req)
	return hosts.Reply(req.ID, value, err)
}
func (r *Runtime) handle(ctx context.Context, c Caller, req hosts.Request) (any, error) {
	if err := r.caller(c); err != nil {
		return nil, err
	}
	switch req.Method {
	case "session.open":
		var p struct {
			Scope []string `json:"scope,omitempty"`
		}
		if err := hosts.Decode(req.Params, &p); err != nil {
			return nil, err
		}
		return r.OpenScoped(c, p.Scope)
	case "session.resume":
		var p struct {
			ID      string `json:"session_id"`
			Epoch   string `json:"runtime_epoch"`
			Token   string `json:"resume_token"`
			Replace bool   `json:"replace_connection,omitempty"`
		}
		if err := hosts.Decode(req.Params, &p); err != nil {
			return nil, err
		}
		return r.ResumeScoped(c, p.ID, p.Epoch, p.Token, p.Replace)
	case "session.close":
		var p struct {
			ID string `json:"session_id"`
		}
		if err := hosts.Decode(req.Params, &p); err != nil {
			return nil, err
		}
		err := r.CloseSession(c, p.ID)
		return map[string]any{"closed": err == nil}, err
	case "catalog.get":
		var p struct {
			SessionID string `json:"session_id"`
		}
		if err := hosts.Decode(req.Params, &p); err != nil {
			return nil, err
		}
		if err := r.CheckSession(c, p.SessionID); err != nil {
			return nil, err
		}
		scoped, err := r.ScopedCaller(c, p.SessionID)
		if err != nil {
			return nil, err
		}
		revision, commands, err := r.Catalog(scoped)
		return map[string]any{"revision": revision, "commands": commands}, err
	case "request":
		var p RequestCall
		if err := hosts.Decode(req.Params, &p); err != nil {
			return nil, err
		}
		return r.Request(ctx, c, p)
	case "invoke":
		var p hosts.Invocation
		if err := hosts.Decode(req.Params, &p); err != nil {
			return nil, err
		}
		return r.Invoke(ctx, c, p)
	case "operation.get":
		var p struct {
			SessionID string `json:"session_id"`
			ID        string `json:"operation_id"`
			WaitMS    int64  `json:"wait_ms,omitempty"`
		}
		if err := hosts.Decode(req.Params, &p); err != nil {
			return nil, err
		}
		if p.WaitMS < 0 || p.WaitMS > 25000 {
			return nil, hosts.Fail("invalid_argument", "Invalid wait_ms")
		}
		return r.Get(ctx, c, p.SessionID, p.ID, time.Duration(p.WaitMS)*time.Millisecond)
	case "operation.cancel":
		var p struct {
			SessionID string `json:"session_id"`
			ID        string `json:"operation_id"`
		}
		if err := hosts.Decode(req.Params, &p); err != nil {
			return nil, err
		}
		return r.Cancel(c, p.SessionID, p.ID)
	default:
		return nil, hosts.Fail("unsupported", "Core method is not implemented")
	}
}

// Dispatch is useful for authenticated byte-oriented transports and tests.
// Large-result byte indirection belongs to the transport's Bytes integration;
// until available, an oversized response is explicitly rejected, not truncated.
func (r *Runtime) Dispatch(ctx context.Context, c Caller, raw []byte) []byte {
	response := r.Handle(ctx, c, raw)
	data, err := json.Marshal(response)
	if err != nil || len(data) > hosts.MaxControlBytes {
		fault := hosts.Fail("overloaded", "Response requires a byte stream")
		if response.OK {
			fault.Retry = "query_operation"
			fault.Effect = "unknown"
		}
		data, _ = json.Marshal(hosts.Reply(response.ID, nil, fault))
	}
	return data
}
