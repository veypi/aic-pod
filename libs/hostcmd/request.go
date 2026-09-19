package hostcmd

import (
	"context"
	"encoding/json"
	"time"

	"github.com/veypi/aic-pod/protocol/hosts"
)

// RequestCall is for bounded observations and sequenced live controls. Durable
// mutations use Invoke. A transport must never retry Request automatically.
type RequestCall struct {
	SessionID    string          `json:"session_id"`
	RuntimeEpoch string          `json:"runtime_epoch"`
	Command      string          `json:"command"`
	Method       string          `json:"method"`
	Args         json.RawMessage `json:"args"`
}

func (r *Runtime) Request(ctx context.Context, c Caller, in RequestCall) (value any, err error) {
	if !hosts.ValidID(in.SessionID) || !hosts.ValidName(in.Command) || !hosts.ValidName(in.Method) {
		return nil, hosts.Fail("invalid_argument", "Invalid request identity")
	}
	if in.RuntimeEpoch != r.epoch {
		return nil, hosts.Fail("expired", "Runtime epoch changed")
	}
	if err = r.caller(c); err != nil {
		return nil, err
	}
	args, err := hosts.CanonicalArgs(in.Args)
	if err != nil {
		return nil, err
	}
	call := Call{Caller: c, SessionID: in.SessionID, Command: in.Command, Method: in.Method, Args: args}
	if err = r.authorize(ctx, call); err != nil {
		return nil, err
	}
	r.mu.Lock()
	s, err := r.bound(c, in.SessionID)
	if err != nil {
		r.mu.Unlock()
		return nil, err
	}
	p, ok := r.providers[in.Command]
	method, exists := p.Descriptor.Methods[in.Method]
	if !ok || !exists || method.Mode != "request" {
		r.mu.Unlock()
		return nil, hosts.Fail("unsupported", "Method does not support transient requests")
	}
	active := 0
	for _, session := range r.sessions {
		active += session.active
	}
	if s.closing || active >= r.cfg.MaxInFlight {
		r.mu.Unlock()
		return nil, hosts.Fail("overloaded", "Runtime request limit reached")
	}
	s.active++
	r.workers.Add(1)
	r.mu.Unlock()
	defer func() {
		if recover() != nil {
			err = hosts.Fail("internal", "Provider panicked")
			err.(*hosts.Fault).Effect = "unknown"
		}
		r.mu.Lock()
		s.active--
		clean := s.closing && s.active == 0
		if clean {
			delete(r.sessions, s.id)
		}
		r.mu.Unlock()
		if clean {
			r.cleanup(s.id)
		}
		r.workers.Done()
	}()
	if err = p.Validate(in.Method, args); err != nil {
		return nil, err
	}
	// Shared capacity bounds transient calls without growing the operation ledger.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	select {
	case r.capacity <- struct{}{}:
		defer func() { <-r.capacity }()
	case <-ctx.Done():
		return nil, hosts.Fail("deadline_exceeded", "Request expired")
	}
	// Request providers own per-resource ordering; the descriptor deliberately does
	// not promise Invoke's durable scope lock. Recheck identity/policy after waiting.
	if err = r.CheckSession(c, in.SessionID); err != nil {
		return nil, err
	}
	if err = r.authorize(ctx, call); err != nil {
		return nil, err
	}
	return p.Run(ctx, call)
}
