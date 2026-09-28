package cua

// service.go 是 cua 的业务服务：原生驱动会话、窗口、快照令牌与输入状态。
// 指令面见 vsh.go（hosts-vsh-redesign：不再有 hosts_tool 声明层）。

import (
	"context"
	"sync"
	"time"

	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
	"github.com/veypi/aic-pod/protocol/ui"
)

type Config struct{ Logf func(string, ...any) }
type Service struct {
	driver *cuaMcp
	native *nativeUI
	mu     sync.Mutex
	closed bool
}

func New(cfg Config) *Service {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Service{driver: newCuaMcp(findCuaDriver(), cfg.Logf), native: newNativeUI()}
}

func (s *Service) execute(ctx context.Context, c wire.Caller, op, target string, locator, args map[string]any, after, delivery string) (*ui.Result, error) {
	if err := c.Validate(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil, wire.Fail("closed", "Native tool closed")
	}
	if s.driver.bin == "" {
		return nil, wire.Fail("unavailable", "cua-driver is unavailable")
	}
	if delivery == "" {
		delivery = "background"
	}
	if after == "" {
		after = "none"
	}
	if after == "observation" {
		after = "snapshot"
	}
	if after == "image" {
		after = "screenshot"
	}
	spec, ok := ui.Spec("cua", op)
	if !ok {
		return nil, wire.Fail("unsupported", "Unknown native operation")
	}
	s.driver.callMu.Lock()
	err := s.driver.ensure(ctx)
	s.driver.mu.Lock()
	epoch := s.driver.generation
	s.driver.mu.Unlock()
	s.driver.callMu.Unlock()
	if err != nil {
		return nil, err
	}
	o := &ui.Operation{Domain: "cua", Op: op, Target: target, Locator: locator, Args: args, Options: ui.Options{After: after, Delivery: delivery, Format: "json"}, Spec: spec}
	// The actor workspace and driver's session are cua business state, independent of transport.
	r := s.native.execute(ctx, o, c.Subject, "", epoch, func(ctx context.Context, name string, args map[string]any) (*mcpResult, error) {
		if err := c.Validate(ctx); err != nil {
			return nil, err
		}
		return s.driver.callEpoch(ctx, epoch, name, args)
	})
	if r.Error != nil {
		return nil, &wire.Fault{Code: r.Error.Code, Message: r.Error.Message, Details: r}
	}
	return r, nil
}
func (s *Service) Status(ctx context.Context, c wire.Caller, a Empty) (map[string]any, error) {
	path := s.driver.bin
	state := "stopped"
	s.driver.mu.Lock()
	if s.driver.alive {
		state = "ready"
	}
	s.driver.mu.Unlock()
	if path == "" {
		state = "unavailable"
	}
	return map[string]any{"state": state, "driver": path}, nil
}
func (s *Service) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	select {
	case s.native.gate <- struct{}{}:
		for _, actor := range s.native.sessions {
			if ctx.Err() != nil {
				break
			}
			_, _ = s.driver.callEpoch(ctx, s.native.epoch, "end_session", map[string]any{"session": actor.driverSession})
		}
		s.native.sessions = map[string]*nativeSession{}
		<-s.native.gate
	case <-ctx.Done():
	}
	s.driver.mu.Lock()
	s.driver.killLocked()
	s.driver.mu.Unlock()
	return nil
}

type ImagePart struct {
	Bytes  []byte `json:"bytes"`
	Offset int    `json:"offset"`
	Total  int    `json:"total"`
	EOF    bool   `json:"eof"`
}

func (s *Service) Image(ctx context.Context, c wire.Caller, a ImageArgs) (ImagePart, error) {
	select {
	case s.native.gate <- struct{}{}:
		defer func() { <-s.native.gate }()
	case <-ctx.Done():
		return ImagePart{}, ctx.Err()
	}
	actor := s.native.sessions[c.Subject]
	if actor == nil {
		return ImagePart{}, wire.Fail("not_found", "Observation expired")
	}
	target := actor.targets[a.WindowID]
	if target == nil || target.snapshot == nil || target.snapshot.id != a.ImageID || len(target.snapshot.image) == 0 {
		return ImagePart{}, wire.Fail("not_found", "Image expired")
	}
	data := target.snapshot.image
	if a.Offset < 0 || a.Offset > len(data) || a.Limit < 0 || a.Limit > 32<<10 {
		return ImagePart{}, wire.Fail("invalid_argument", "Invalid image range")
	}
	if a.Limit == 0 {
		a.Limit = 32 << 10
	}
	end := min(a.Offset+a.Limit, len(data))
	return ImagePart{Bytes: data[a.Offset:end], Offset: a.Offset, Total: len(data), EOF: end == len(data)}, nil
}
