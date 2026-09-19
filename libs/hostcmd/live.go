package hostcmd

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/veypi/aic-pod/protocol/hosts"
)

const MaxLiveBytes = 8 << 20

// Live carries ephemeral state and ordered user events. Recv applies whole-item
// backpressure; providers retain the latest state, not a queue of old frames.
type LiveItem struct {
	Metadata json.RawMessage
	Data     []byte
}
type Live interface {
	Recv() (LiveItem, error)
	Send(json.RawMessage) error
	Close() error
}
type runtimeLive struct {
	Live
	r      *Runtime
	s      *session
	call   Call
	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once
}

func (l *runtimeLive) check() error {
	if err := l.ctx.Err(); err != nil {
		return err
	}
	// Authorize refreshes the connection policy, including a renewed RTC ticket.
	if err := l.r.authorize(l.ctx, l.call); err != nil {
		return err
	}
	l.r.mu.Lock()
	defer l.r.mu.Unlock()
	s, err := l.r.bound(l.call.Caller, l.call.SessionID)
	if err != nil {
		return err
	}
	if s.closing {
		return hosts.Fail("expired", "Session closed")
	}
	return nil
}
func (l *runtimeLive) Recv() (item LiveItem, err error) {
	defer livePanic(&err)
	if err := l.check(); err != nil {
		return LiveItem{}, err
	}
	item, err = l.Live.Recv()
	if err == nil {
		err = l.check()
	}
	return item, err
}
func (l *runtimeLive) Send(data json.RawMessage) (err error) {
	defer livePanic(&err)
	if err := l.check(); err != nil {
		return err
	}
	if _, err := hosts.CanonicalArgs(data); err != nil {
		return err
	}
	return l.Live.Send(data)
}
func (l *runtimeLive) Close() error {
	l.once.Do(func() {
		l.cancel()
		closeLiveProvider(l.Live)
		l.r.mu.Lock()
		delete(l.s.live, l)
		l.s.active--
		clean := l.s.closing && l.s.active == 0
		if clean {
			delete(l.r.sessions, l.s.id)
		}
		l.r.mu.Unlock()
		if clean {
			l.r.cleanup(l.s.id)
		}
		l.r.workers.Done()
	})
	return nil
}
func (r *Runtime) OpenLive(ctx context.Context, c Caller, in RequestCall) (Live, error) {
	if !hosts.ValidID(in.SessionID) || !hosts.ValidName(in.Command) || !hosts.ValidName(in.Method) {
		return nil, hosts.Fail("invalid_argument", "Invalid live method")
	}
	if in.RuntimeEpoch != r.epoch {
		return nil, hosts.Fail("expired", "Runtime epoch changed")
	}
	if err := r.CheckSession(c, in.SessionID); err != nil {
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
	p, ok := r.providers[in.Command]
	r.mu.Unlock()
	if !ok || p.Descriptor.Methods[in.Method].Mode != "live" || p.OpenLive == nil {
		return nil, hosts.Fail("unsupported", "Method does not support live streams")
	}
	if err = p.Validate(in.Method, args); err != nil {
		return nil, err
	}
	// Count admission before opening so shutdown and session close fence opens.
	r.mu.Lock()
	s, err := r.bound(c, in.SessionID)
	if err != nil {
		r.mu.Unlock()
		return nil, err
	}
	active := 0
	for _, s := range r.sessions {
		active += s.active
	}
	if s.closing || active >= r.cfg.MaxInFlight {
		r.mu.Unlock()
		return nil, hosts.Fail("overloaded", "Live stream limit reached")
	}
	s.active++
	r.workers.Add(1)
	r.mu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	live, err := openLiveProvider(p, ctx, call)
	if err != nil {
		cancel()
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
		return nil, err
	}
	l := &runtimeLive{Live: live, r: r, s: s, call: call, ctx: ctx, cancel: cancel}
	r.mu.Lock()
	if s.live == nil {
		s.live = map[*runtimeLive]struct{}{}
	}
	s.live[l] = struct{}{}
	r.mu.Unlock()
	if err = l.check(); err != nil {
		l.Close()
		return nil, err
	}
	context.AfterFunc(ctx, func() { l.Close() })
	return l, nil
}

func livePanic(err *error) {
	if recover() != nil {
		f := hosts.Fail("internal", "Live provider panicked")
		f.Effect = "unknown"
		*err = f
	}
}
func openLiveProvider(p Provider, ctx context.Context, call Call) (live Live, err error) {
	defer livePanic(&err)
	live, err = p.OpenLive(ctx, call)
	if err == nil && live == nil {
		err = hosts.Fail("internal", "Live provider returned no stream")
	}
	return live, err
}
func closeLiveProvider(live Live) (err error) { defer livePanic(&err); return live.Close() }
