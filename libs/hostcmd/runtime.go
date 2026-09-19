// Package hostcmd owns transport-independent device command admission, sessions,
// cancellation and deduplication. A transport supplies a verified Caller; it may
// never decode Caller or granted privileges from command arguments.
package hostcmd

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/veypi/aic-pod/protocol/hosts"
)

type Caller struct {
	Subject      string
	ConnectionID string
	ExpiresAt    time.Time
	Scopes       []string
	Transport    string
}
type Call struct {
	Caller      Caller
	SessionID   string
	OperationID string
	Command     string
	Method      string
	Args        json.RawMessage
}
type Provider struct {
	Descriptor hosts.Command
	Validate   func(method string, args json.RawMessage) error
	// Scope is device-derived. Empty permits concurrency; equal scopes serialize
	// reads/writes until the actual handler returns, including after cancellation.
	Scope    func(Call) string
	Run      func(context.Context, Call) (any, error)
	OpenLive func(context.Context, Call) (Live, error)
}
type Config struct {
	Authorize func(context.Context, Call) error
	// Reauthorize checks live policy for already-admitted operations, independently of a transport lease.
	Reauthorize    func(context.Context, Call) error
	ResultPolicy   func(Call) (func(context.Context) error, error)
	SessionTTL     time.Duration
	MaxSessions    int
	MaxOperations  int
	MaxConcurrent  int
	MaxInFlight    int
	DefaultTimeout time.Duration
	Now            func() time.Time
	OnSessionClose func(string)
}
type Session struct {
	ID           string `json:"session_id"`
	RuntimeEpoch string `json:"runtime_epoch"`
	ResumeToken  string `json:"resume_token"`
	ResumeTTLMS  int64  `json:"resume_ttl_ms"`
}
type session struct {
	id, subject, connection string
	token                   [32]byte
	previousToken           [32]byte
	lastResumeToken         string
	scopes                  []string
	disconnected            time.Time
	closing                 bool
	operations              map[string]*operation
	active                  int
	live                    map[*runtimeLive]struct{}
}
type operation struct {
	call   Call
	state  hosts.Operation
	digest [32]byte
	done   chan struct{}
	cancel context.CancelFunc
}
type gate struct {
	token chan struct{}
	refs  int
}
type Runtime struct {
	cfg       Config
	epoch     string
	mu        sync.Mutex
	providers map[string]Provider
	sessions  map[string]*session
	gates     map[string]*gate
	capacity  chan struct{}
	closed    bool
	workers   sync.WaitGroup
	revision  uint64
}

func New(cfg Config) (*Runtime, error) {
	if cfg.Authorize == nil {
		return nil, fmt.Errorf("hostcmd: an authorizer is required")
	}
	if cfg.Reauthorize == nil {
		cfg.Reauthorize = cfg.Authorize
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 10 * time.Minute
	}
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = 32
	}
	if cfg.MaxOperations <= 0 {
		cfg.MaxOperations = 4096
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 16
	}
	if cfg.MaxInFlight <= 0 {
		cfg.MaxInFlight = 128
	}
	if cfg.DefaultTimeout <= 0 {
		cfg.DefaultTimeout = 30 * time.Second
	}
	if cfg.DefaultTimeout > 5*time.Minute {
		return nil, fmt.Errorf("hostcmd: timeout exceeds 5 minutes")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	epoch, err := hosts.NewID("rt_")
	if err != nil {
		return nil, err
	}
	return &Runtime{cfg: cfg, epoch: epoch, providers: map[string]Provider{}, sessions: map[string]*session{}, gates: map[string]*gate{}, capacity: make(chan struct{}, cfg.MaxConcurrent)}, nil
}
func (r *Runtime) Epoch() string { return r.epoch }

// CheckSession is used by byte/resource endpoints before accessing a handle.
func (r *Runtime) CheckSession(c Caller, id string) error {
	if err := r.caller(c); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s, err := r.bound(c, id)
	if err != nil {
		return err
	}
	if s.closing {
		return hosts.Fail("expired", "Session is closing")
	}
	return nil
}
func (r *Runtime) Register(p Provider) error {
	if err := p.Descriptor.Validate(); err != nil {
		return err
	}
	if p.Validate == nil || p.Run == nil {
		return fmt.Errorf("hostcmd: provider requires validation and execution")
	}
	// Freeze descriptors; provider code must not mutate a published catalog.
	raw, err := json.Marshal(p.Descriptor)
	if err != nil {
		return err
	}
	p.Descriptor = hosts.Command{}
	if err = json.Unmarshal(raw, &p.Descriptor); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return hosts.Fail("expired", "Runtime closed")
	}
	r.providers[p.Descriptor.Name] = p
	r.revision++
	return nil
}
func (r *Runtime) caller(c Caller) error {
	if c.Subject == "" || !hosts.ValidID(c.ConnectionID) || !c.ExpiresAt.After(r.cfg.Now()) {
		return hosts.Fail("unauthorized", "Connection authorization expired")
	}
	return nil
}
func (r *Runtime) Catalog(c Caller) (uint64, []hosts.Command, error) {
	if err := r.caller(c); err != nil {
		return 0, nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, nil, hosts.Fail("expired", "Runtime closed")
	}
	out := make([]hosts.Command, 0, len(r.providers))
	for _, p := range r.providers {
		if len(c.Scopes) > 0 && !slices.Contains(c.Scopes, p.Descriptor.Name) {
			continue
		}
		out = append(out, p.Descriptor)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	raw, _ := json.Marshal(out)
	out = nil
	_ = json.Unmarshal(raw, &out)
	return r.revision, out, nil
}
func (r *Runtime) Open(c Caller) (Session, error) { return r.OpenScoped(c, nil) }
func (r *Runtime) OpenScoped(c Caller, scope []string) (Session, error) {
	if len(scope) == 0 {
		scope = c.Scopes
	}
	if len(scope) > 16 || !scopeContains(c.Scopes, scope) {
		return Session{}, hosts.Fail("permission_denied", "Session scope is not allowed")
	}
	for _, name := range scope {
		if !hosts.ValidName(name) {
			return Session{}, hosts.Fail("invalid_argument", "Invalid session scope")
		}
	}
	r.Reap()
	if err := r.caller(c); err != nil {
		return Session{}, err
	}
	id, err := hosts.NewID("s_")
	if err != nil {
		return Session{}, err
	}
	token, err := hosts.NewID("resume_")
	if err != nil {
		return Session{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return Session{}, hosts.Fail("expired", "Runtime closed")
	}
	if len(r.sessions) >= r.cfg.MaxSessions {
		return Session{}, hosts.Fail("overloaded", "Session limit reached")
	}
	r.sessions[id] = &session{id: id, subject: c.Subject, connection: c.ConnectionID, token: sha256.Sum256([]byte(token)), operations: map[string]*operation{}, scopes: append([]string(nil), scope...)}
	return Session{ID: id, RuntimeEpoch: r.epoch, ResumeToken: token, ResumeTTLMS: r.cfg.SessionTTL.Milliseconds()}, nil
}
func (r *Runtime) Resume(c Caller, id, epoch, token string) (Session, error) {
	return r.ResumeScoped(c, id, epoch, token, false)
}
func (r *Runtime) ResumeScoped(c Caller, id, epoch, token string, replace bool) (Session, error) {
	r.Reap()
	if err := r.caller(c); err != nil {
		return Session{}, err
	}
	digest := sha256.Sum256([]byte(token))
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.sessions[id]
	if r.closed || epoch != r.epoch || s == nil || s.subject != c.Subject || s.closing {
		return Session{}, hosts.Fail("expired", "Session cannot be resumed")
	}
	if !scopeContains(c.Scopes, s.scopes) {
		return Session{}, hosts.Fail("permission_denied", "Session exceeds transport scope")
	}
	valid := subtle.ConstantTimeCompare(s.token[:], digest[:]) == 1
	// A response can be lost after rotation. Only the already-bound connection
	// may repeat that resume with the immediately previous token.
	if !valid && s.connection == c.ConnectionID && s.lastResumeToken != "" && subtle.ConstantTimeCompare(s.previousToken[:], digest[:]) == 1 {
		token = s.lastResumeToken
		valid = true
	}
	if !valid {
		return Session{}, hosts.Fail("expired", "Invalid session recovery token")
	}
	if s.connection != "" && s.connection != c.ConnectionID && (!replace || len(s.scopes) != 1 || s.scopes[0] != "fs") {
		return Session{}, hosts.Fail("session_busy", "Session is bound to another connection")
	}
	if s.connection != c.ConnectionID {
		next, err := hosts.NewID("resume_")
		if err != nil {
			return Session{}, err
		}
		s.previousToken = s.token
		s.token = sha256.Sum256([]byte(next))
		s.lastResumeToken = next
		token = next
	}
	s.connection = c.ConnectionID
	s.disconnected = time.Time{}
	return Session{ID: s.id, RuntimeEpoch: r.epoch, ResumeToken: token, ResumeTTLMS: r.cfg.SessionTTL.Milliseconds()}, nil
}
func (r *Runtime) bound(c Caller, id string) (*session, error) {
	if r.closed {
		return nil, hosts.Fail("expired", "Runtime closed")
	}
	s := r.sessions[id]
	if s == nil || s.subject != c.Subject || s.connection != c.ConnectionID {
		return nil, hosts.Fail("expired", "Session is not bound to this connection")
	}
	if !scopeContains(c.Scopes, s.scopes) {
		return nil, hosts.Fail("permission_denied", "Session exceeds transport scope")
	}
	return s, nil
}
func (r *Runtime) Disconnect(connection string) {
	r.mu.Lock()
	var live []*runtimeLive
	for _, s := range r.sessions {
		if s.connection == connection {
			s.connection = ""
			s.disconnected = r.cfg.Now()
			for l := range s.live {
				live = append(live, l)
			}
		}
	}
	r.mu.Unlock()
	for _, l := range live {
		l.Close()
	}
}
func (r *Runtime) CloseSession(c Caller, id string) error {
	if err := r.caller(c); err != nil {
		return err
	}
	r.mu.Lock()
	if r.sessions[id] == nil {
		r.mu.Unlock()
		return nil
	}
	s, err := r.bound(c, id)
	if err != nil {
		r.mu.Unlock()
		return err
	}
	s.closing = true
	var live []*runtimeLive
	for l := range s.live {
		live = append(live, l)
	}
	clean := s.active == 0
	if clean {
		delete(r.sessions, id)
	}
	r.mu.Unlock()
	for _, l := range live {
		l.Close()
	}
	if clean {
		r.cleanup(id)
	}
	return nil
}
func (r *Runtime) cleanup(id string) {
	if r.cfg.OnSessionClose != nil {
		r.cfg.OnSessionClose(id)
	}
}
func (r *Runtime) Reap() {
	var expired []string
	r.mu.Lock()
	for id, s := range r.sessions {
		if s.active == 0 && (s.closing || (!s.disconnected.IsZero() && r.cfg.Now().Sub(s.disconnected) >= r.cfg.SessionTTL)) {
			delete(r.sessions, id)
			expired = append(expired, id)
		}
	}
	r.mu.Unlock()
	for _, id := range expired {
		r.cleanup(id)
	}
}
func (r *Runtime) Invoke(ctx context.Context, c Caller, in hosts.Invocation) (hosts.Operation, error) {
	if err := r.caller(c); err != nil {
		return hosts.Operation{}, err
	}
	if err := in.Validate(); err != nil {
		return hosts.Operation{}, err
	}
	if in.RuntimeEpoch != r.epoch {
		return hosts.Operation{}, hosts.Fail("expired", "Runtime epoch changed; do not replay this operation")
	}
	args, err := hosts.CanonicalArgs(in.Args)
	if err != nil {
		return hosts.Operation{}, err
	}
	in.Args = args
	// Parameters are immutable and timeout is part of the operation identity.
	identity, _ := json.Marshal(in)
	digest := sha256.Sum256(identity)
	call := Call{Caller: c, SessionID: in.SessionID, OperationID: in.OperationID, Command: in.Command, Method: in.Method, Args: args}
	if err = r.authorize(ctx, call); err != nil {
		return hosts.Operation{}, err
	}
	r.mu.Lock()
	s, err := r.bound(c, in.SessionID)
	if err != nil {
		r.mu.Unlock()
		return hosts.Operation{}, err
	}
	if old := s.operations[in.OperationID]; old != nil {
		if old.digest != digest {
			r.mu.Unlock()
			return hosts.Operation{}, hosts.Fail("operation_conflict", "Operation ID already has different parameters")
		}
		state := clone(old.state)
		r.mu.Unlock()
		return state, nil
	}
	if s.closing {
		r.mu.Unlock()
		return hosts.Operation{}, hosts.Fail("expired", "Session is closing")
	}
	p, ok := r.providers[in.Command]
	if !ok {
		r.mu.Unlock()
		return hosts.Operation{}, hosts.Fail("unsupported", "Command is not registered")
	}
	if method, exists := p.Descriptor.Methods[in.Method]; !exists || method.Mode != "" {
		r.mu.Unlock()
		return hosts.Operation{}, hosts.Fail("unsupported", "Method is not registered")
	}
	r.mu.Unlock()
	if err = p.Validate(in.Method, args); err != nil {
		return hosts.Operation{}, err
	}
	if err = ctx.Err(); err != nil {
		return hosts.Operation{}, hosts.Fail("cancelled", "Request cancelled before admission")
	}
	scope := ""
	if p.Scope != nil {
		scope = p.Scope(call)
	}
	timeout := r.cfg.DefaultTimeout
	if in.TimeoutMS > 0 {
		timeout = time.Duration(in.TimeoutMS) * time.Millisecond
	}
	// Deliberately not a child of the network request: disconnect is not cancel.
	runCtx, cancel := context.WithTimeout(context.Background(), timeout)
	r.mu.Lock()
	s, err = r.bound(c, in.SessionID)
	if err != nil {
		r.mu.Unlock()
		cancel()
		return hosts.Operation{}, err
	}
	if old := s.operations[in.OperationID]; old != nil {
		state := clone(old.state)
		r.mu.Unlock()
		cancel()
		if old.digest != digest {
			return hosts.Operation{}, hosts.Fail("operation_conflict", "Operation ID already has different parameters")
		}
		return state, nil
	}
	inFlight, retained := 0, 0
	for _, current := range r.sessions {
		inFlight += current.active
		retained += len(current.operations)
	}
	if s.closing || retained >= r.cfg.MaxOperations || inFlight >= r.cfg.MaxInFlight {
		r.mu.Unlock()
		cancel()
		return hosts.Operation{}, hosts.Fail("overloaded", "Session is closing or the runtime operation limit was reached")
	}
	op := &operation{call: call, state: hosts.Operation{ID: in.OperationID, Status: "accepted"}, digest: digest, done: make(chan struct{}), cancel: cancel}
	s.operations[in.OperationID] = op
	s.active++
	var g *gate
	if scope != "" {
		g = r.gates[scope]
		if g == nil {
			g = &gate{token: make(chan struct{}, 1)}
			r.gates[scope] = g
		}
		g.refs++
	}
	r.workers.Add(1)
	state := clone(op.state)
	r.mu.Unlock()
	go r.execute(runCtx, s, op, p, call, scope, g)
	return state, nil
}
func clone(in hosts.Operation) hosts.Operation {
	raw, _ := json.Marshal(in)
	var out hosts.Operation
	_ = json.Unmarshal(raw, &out)
	return out
}
func (r *Runtime) execute(ctx context.Context, s *session, op *operation, p Provider, call Call, scope string, g *gate) {
	defer r.workers.Done()
	defer op.cancel()
	var value any
	var err error
	started := false
	acquire := func(ch chan struct{}) bool {
		select {
		case ch <- struct{}{}:
			return true
		case <-ctx.Done():
			err = ctx.Err()
			return false
		}
	}
	func() {
		defer func() {
			if recover() != nil {
				err = hosts.Fail("internal", "Provider panicked")
				err.(*hosts.Fault).Effect = "unknown"
			}
		}()
		if g != nil {
			if !acquire(g.token) {
				return
			}
			defer func() { <-g.token }()
		}
		if !acquire(r.capacity) {
			return
		}
		defer func() { <-r.capacity }()
		if err = ctx.Err(); err != nil {
			return
		}
		// The admitted session scope is immutable. A network disconnect or
		// transport handoff cannot revoke admission; mutable device policy is
		// still rechecked immediately before execution.
		if err = r.cfg.Reauthorize(ctx, call); err != nil {
			return
		}
		r.mu.Lock()
		op.state.Status = "running"
		r.mu.Unlock()
		started = true
		value, err = p.Run(ctx, call)
	}()
	var encoded json.RawMessage
	if err == nil {
		encoded, err = json.Marshal(value)
	}
	r.mu.Lock()
	if err == nil {
		op.state.Status = "succeeded"
		op.state.Value = encoded
	} else {
		op.state.Status = "failed"
		f := hosts.AsFault(err)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			f = hosts.Fail("cancelled", "Operation cancellation completed")
			if errors.Is(err, context.DeadlineExceeded) {
				f.Code = "deadline_exceeded"
				f.Message = "Operation deadline elapsed"
			}
			if started {
				f.Effect = "unknown"
			}
			op.state.Status = "cancelled"
		} else if f.Code == "cancelled" || f.Code == "deadline_exceeded" {
			// Providers can report a confirmed cancellation with explicit
			// partial effects; keep that information instead of guessing.
			op.state.Status = "cancelled"
		} else if f.Code == "internal" && started && f.Effect == "none" {
			f.Effect = "unknown"
		}
		op.state.Error = f
	}
	s.active--
	if !s.disconnected.IsZero() && s.active == 0 {
		s.disconnected = r.cfg.Now()
	}
	if g != nil {
		g.refs--
		if g.refs == 0 {
			delete(r.gates, scope)
		}
	}
	clean := s.closing && s.active == 0
	if clean {
		delete(r.sessions, s.id)
	}
	close(op.done)
	r.mu.Unlock()
	if clean {
		r.cleanup(s.id)
	}
}
func (r *Runtime) Get(ctx context.Context, c Caller, sessionID, id string, wait time.Duration) (hosts.Operation, error) {
	if err := r.caller(c); err != nil {
		return hosts.Operation{}, err
	}
	if wait < 0 || wait > 25*time.Second {
		return hosts.Operation{}, hosts.Fail("invalid_argument", "Invalid operation wait window")
	}
	r.mu.Lock()
	s, err := r.bound(c, sessionID)
	if err != nil {
		r.mu.Unlock()
		return hosts.Operation{}, err
	}
	op := s.operations[id]
	if op == nil {
		r.mu.Unlock()
		return hosts.Operation{}, hosts.Fail("not_accepted", "Operation was not accepted in this session")
	}
	done := op.done
	call := op.call
	call.Caller = c
	r.mu.Unlock()
	if err := r.authorize(ctx, call); err != nil {
		return hosts.Operation{}, err
	}
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
		case <-ctx.Done():
			return hosts.Operation{}, ctx.Err()
		}
	}
	if err := r.caller(c); err != nil {
		return hosts.Operation{}, err
	}
	if err := r.authorize(ctx, call); err != nil {
		return hosts.Operation{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err = r.bound(c, sessionID); err != nil {
		return hosts.Operation{}, err
	}
	return clone(op.state), nil
}
func (r *Runtime) Cancel(c Caller, sessionID, id string) (hosts.Operation, error) {
	if err := r.caller(c); err != nil {
		return hosts.Operation{}, err
	}
	r.mu.Lock()
	s, err := r.bound(c, sessionID)
	if err != nil {
		r.mu.Unlock()
		return hosts.Operation{}, err
	}
	op := s.operations[id]
	if op == nil {
		r.mu.Unlock()
		return hosts.Operation{}, hosts.Fail("not_accepted", "Operation was not accepted")
	}
	call := op.call
	call.Caller = c
	r.mu.Unlock()
	if err = r.authorize(context.Background(), call); err != nil {
		return hosts.Operation{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err = r.bound(c, sessionID); err != nil {
		return hosts.Operation{}, err
	}
	if !op.state.Terminal() {
		op.state.CancelRequested = true
		op.cancel()
	}
	return clone(op.state), nil
}

// An empty scope is unrestricted only for a trusted transport or an explicitly
// unrestricted RTC session; it never means unrestricted on proxy.
func scopeContains(allowed, requested []string) bool {
	if len(allowed) == 0 {
		return true
	}
	if len(requested) == 0 {
		return false
	}
	for _, name := range requested {
		if !slices.Contains(allowed, name) {
			return false
		}
	}
	return true
}
func (r *Runtime) authorize(ctx context.Context, call Call) error {
	r.mu.Lock()
	s, err := r.bound(call.Caller, call.SessionID)
	if err == nil && len(s.scopes) > 0 && !slices.Contains(s.scopes, call.Command) {
		err = hosts.Fail("permission_denied", "Command is outside the session scope")
	}
	r.mu.Unlock()
	if err != nil {
		return err
	}
	return r.cfg.Authorize(ctx, call)
}

// ResultPolicy captures the admitted call for a large result's byte source.
// The source remains session-bound by Bytes; every read also rechecks live policy.
func (r *Runtime) ResultPolicy(c Caller, sessionID, operationID string) (func(context.Context) error, error) {
	r.mu.Lock()
	s, err := r.bound(c, sessionID)
	if err != nil {
		r.mu.Unlock()
		return nil, err
	}
	op := s.operations[operationID]
	if op == nil {
		r.mu.Unlock()
		return nil, hosts.Fail("not_accepted", "Operation was not accepted")
	}
	call := op.call
	r.mu.Unlock()
	if r.cfg.ResultPolicy != nil {
		return r.cfg.ResultPolicy(call)
	}
	return func(ctx context.Context) error { return r.cfg.Reauthorize(ctx, call) }, nil
}
func (r *Runtime) ScopedCaller(c Caller, id string) (Caller, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, err := r.bound(c, id)
	if err != nil {
		return c, err
	}
	c.Scopes = append([]string(nil), s.scopes...)
	return c, nil
}
func (r *Runtime) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	r.closed = true
	var live []*runtimeLive
	for _, s := range r.sessions {
		for l := range s.live {
			live = append(live, l)
		}
		s.closing = true
		for _, op := range s.operations {
			if !op.state.Terminal() {
				op.cancel()
			}
		}
	}
	r.mu.Unlock()
	for _, l := range live {
		l.Close()
	}
	done := make(chan struct{})
	go func() { r.workers.Wait(); close(done) }()
	select {
	case <-done:
		r.Reap()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
