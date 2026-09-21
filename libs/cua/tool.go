// Package cua owns native driver sessions, windows, snapshot tokens and input state.
package cua

import (
	"context"
	"encoding/json"
	tool "github.com/veypi/aic-pod/libs/hosts_tool"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
	"github.com/veypi/aic-pod/protocol/ui"
	"sync"
	"time"
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

type Empty struct{}
type WindowArgs struct {
	WindowID string `json:"window_id" required:"true"`
}
type ListArgs struct {
	App string `json:"app,omitempty"`
	PID int    `json:"pid,omitempty"`
}
type OpenArgs struct {
	App string `json:"app" required:"true"`
}
type ObserveArgs struct {
	WindowID string `json:"window_id" required:"true"`
	Image    bool   `json:"image,omitempty"`
	Query    string `json:"query,omitempty"`
	Depth    int    `json:"depth,omitempty"`
}
type Locator struct {
	Ref      string    `json:"ref,omitempty"`
	Role     string    `json:"role,omitempty"`
	Name     string    `json:"name,omitempty"`
	Label    string    `json:"label,omitempty"`
	At       []float64 `json:"at,omitempty"`
	Snapshot string    `json:"snapshot,omitempty"`
}

func (l Locator) Validate() error {
	n := 0
	for _, v := range []string{l.Ref, l.Role, l.Label} {
		if v != "" {
			n++
		}
	}
	if len(l.At) > 0 {
		n++
		if len(l.At) != 2 || l.Snapshot == "" {
			return wire.Fail("invalid_argument", "Coordinates require two values and snapshot")
		}
	}
	if n != 1 || l.Name != "" && l.Role == "" {
		return wire.Fail("invalid_argument", "Provide one locator: ref, role/name, label or snapshot coordinates")
	}
	return nil
}

type ActionArgs struct {
	WindowID string  `json:"window_id" required:"true"`
	Locator  Locator `json:"locator" required:"true"`
	Text     string  `json:"text,omitempty"`
	Key      string  `json:"key,omitempty"`
	Value    any     `json:"value,omitempty"`
	X        float64 `json:"x,omitempty"`
	Y        float64 `json:"y,omitempty"`
	Button   string  `json:"button,omitempty" enum:"left,right,middle"`
	Count    int     `json:"count,omitempty"`
	Delivery string  `json:"delivery,omitempty" enum:"background,foreground"`
	After    string  `json:"after,omitempty" enum:"none,observation,image"`
}

func (a *ActionArgs) Validate() error {
	if !wire.ValidID(a.WindowID) || a.Count < 0 || a.Count > 2 {
		return wire.Fail("invalid_argument", "Invalid window or click count")
	}
	return a.Locator.Validate()
}

type WaitArgs struct {
	WindowID string   `json:"window_id" required:"true"`
	Locator  *Locator `json:"locator,omitempty"`
	Text     *string  `json:"text,omitempty"`
	State    string   `json:"state,omitempty" enum:"visible,hidden,enabled,disabled"`
}
type DragArgs struct {
	WindowID string    `json:"window_id" required:"true"`
	Snapshot string    `json:"snapshot" required:"true"`
	From     []float64 `json:"from_at" required:"true"`
	To       []float64 `json:"to_at" required:"true"`
	Delivery string    `json:"delivery,omitempty" enum:"background,foreground"`
}
type MenuArgs struct {
	WindowID string   `json:"window_id" required:"true"`
	Path     []string `json:"path" required:"true"`
}
type BoundsArgs struct {
	WindowID string  `json:"window_id" required:"true"`
	X        float64 `json:"x" required:"true"`
	Y        float64 `json:"y" required:"true"`
	Width    float64 `json:"width" required:"true"`
	Height   float64 `json:"height" required:"true"`
}
type TextArgs struct {
	Text string `json:"text" required:"true"`
}
type CursorArgs struct {
	Enabled bool `json:"enabled" required:"true"`
}
type ImageArgs struct {
	Offset   int    `json:"offset,omitempty"`
	Limit    int    `json:"limit,omitempty"`
	WindowID string `json:"window_id" required:"true"`
	ImageID  string `json:"image_id" required:"true"`
}

func fields(v any) map[string]any {
	b, _ := json.Marshal(v)
	out := map[string]any{}
	_ = json.Unmarshal(b, &out)
	return out
}
func (s *Service) execute(ctx context.Context, c tool.Caller, op, target string, locator, args map[string]any, after, delivery string) (*ui.Result, error) {
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
func (s *Service) Status(ctx context.Context, c tool.Caller, a Empty) (map[string]any, error) {
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
func (s *Service) Tool() tool.Command {
	spec := func(name, cli string, level int, pos ...string) tool.Spec {
		value := tool.Spec{Name: name, CLI: cli, Access: level, Positionals: pos}
		switch name {
		case "window.click", "window.fill", "window.type", "window.press", "window.scroll", "window.set", "window.move", "window.drag":
			value.AccessRules = []wire.AccessRule{{Field: "delivery", Equals: "foreground", Level: 3}}
		}
		return value
	}
	t := tool.DefineCommand("cua",
		tool.Bind(spec("status", "status", 1), s.Status),
		tool.Bind(spec("app.list", "apps", 1), func(ctx context.Context, c tool.Caller, a Empty) (*ui.Result, error) {
			return s.execute(ctx, c, "apps", "", nil, map[string]any{}, "none", "")
		}),
		tool.Bind(spec("app.open", "open", 2, "app"), func(ctx context.Context, c tool.Caller, a OpenArgs) (*ui.Result, error) {
			return s.execute(ctx, c, "open", "", nil, fields(a), "none", "")
		}),
		tool.Bind(spec("window.list", "windows", 1), func(ctx context.Context, c tool.Caller, a ListArgs) (*ui.Result, error) {
			return s.execute(ctx, c, "target.list", "", nil, fields(a), "none", "")
		}),
		tool.Bind(spec("window.observe", "observe", 1), func(ctx context.Context, c tool.Caller, a ObserveArgs) (*ui.Result, error) {
			if !wire.ValidID(a.WindowID) || a.Depth < 0 || a.Depth > 100 {
				return nil, wire.Fail("invalid_argument", "Invalid window or depth")
			}
			return s.execute(ctx, c, "snapshot", a.WindowID, nil, fields(a), "none", "")
		}),
		tool.Bind(spec("window.activate", "activate", 3), func(ctx context.Context, c tool.Caller, a WindowArgs) (*ui.Result, error) {
			return s.execute(ctx, c, "activate", a.WindowID, nil, map[string]any{}, "none", "")
		}),
		tool.Bind(spec("window.menu", "menu", 2), func(ctx context.Context, c tool.Caller, a MenuArgs) (*ui.Result, error) {
			if len(a.Path) == 0 || len(a.Path) > 16 {
				return nil, wire.Fail("invalid_argument", "Expected bounded menu path")
			}
			return s.execute(ctx, c, "menu", a.WindowID, nil, fields(a), "none", "")
		}),
		tool.Bind(spec("window.bounds", "bounds", 2), func(ctx context.Context, c tool.Caller, a BoundsArgs) (*ui.Result, error) {
			if a.Width <= 0 || a.Height <= 0 || a.Width > 16384 || a.Height > 16384 {
				return nil, wire.Fail("invalid_argument", "Invalid bounds")
			}
			return s.execute(ctx, c, "window.bounds", a.WindowID, nil, fields(a), "none", "")
		}),
		tool.Bind(spec("clipboard.read", "clipboard.read", 2), func(ctx context.Context, c tool.Caller, a Empty) (*ui.Result, error) {
			return s.execute(ctx, c, "clipboard.read", "", nil, map[string]any{}, "none", "")
		}),
		tool.Bind(spec("clipboard.write", "clipboard.write", 2, "text"), func(ctx context.Context, c tool.Caller, a TextArgs) (*ui.Result, error) {
			return s.execute(ctx, c, "clipboard.write", "", nil, fields(a), "none", "")
		}),
		tool.Bind(spec("cursor.state", "cursor.state", 1), func(ctx context.Context, c tool.Caller, a Empty) (*ui.Result, error) {
			return s.execute(ctx, c, "cursor.state", "", nil, map[string]any{}, "none", "")
		}),
		tool.Bind(spec("cursor.set", "cursor.set", 2), func(ctx context.Context, c tool.Caller, a CursorArgs) (*ui.Result, error) {
			op := "cursor.off"
			if a.Enabled {
				op = "cursor.on"
			}
			return s.execute(ctx, c, op, "", nil, map[string]any{}, "none", "")
		}),
		tool.Bind(spec("window.wait", "wait", 1), func(ctx context.Context, c tool.Caller, a WaitArgs) (*ui.Result, error) {
			var locator map[string]any
			if (a.Locator == nil) == (a.Text == nil) {
				return nil, wire.Fail("invalid_argument", "Provide text or semantic locator")
			}
			if a.Locator != nil {
				if err := a.Locator.Validate(); err != nil {
					return nil, err
				}
				if a.Locator.Ref != "" || len(a.Locator.At) > 0 || a.State == "" {
					return nil, wire.Fail("invalid_argument", "Wait requires semantic locator and state")
				}
				locator = fields(a.Locator)
			}
			return s.execute(ctx, c, "wait", a.WindowID, locator, fields(a), "none", "")
		}),
		tool.Bind(spec("window.drag", "drag", 2), func(ctx context.Context, c tool.Caller, a DragArgs) (*ui.Result, error) {
			if a.Snapshot == "" || len(a.From) != 2 || len(a.To) != 2 {
				return nil, wire.Fail("invalid_argument", "Drag requires snapshot and two coordinate pairs")
			}
			if a.Delivery == "foreground" && c.Level < 3 {
				return nil, wire.Fail("permission_denied", "Foreground input requires level 3")
			}
			return s.execute(ctx, c, "drag", a.WindowID, nil, fields(a), "none", a.Delivery)
		}),
		tool.Bind(tool.Spec{Name: "observation.image.read", Access: 1}, s.Image),
	)
	for _, name := range []string{"click", "fill", "type", "press", "scroll", "set", "move"} {
		t.Methods = append(t.Methods, tool.Bind(spec("window."+name, name, 2), func(ctx context.Context, c tool.Caller, a ActionArgs) (*ui.Result, error) {
			if a.Delivery == "foreground" && c.Level < 3 {
				return nil, wire.Fail("permission_denied", "Foreground input requires level 3")
			}
			args := fields(a)
			if a.Count == 2 {
				args["count"] = "2"
			}
			return s.execute(ctx, c, name, a.WindowID, fields(a.Locator), args, a.After, a.Delivery)
		}))
	}
	t.Close = s.Close
	return t
}

type ImagePart struct {
	Bytes  []byte `json:"bytes"`
	Offset int    `json:"offset"`
	Total  int    `json:"total"`
	EOF    bool   `json:"eof"`
}

func (s *Service) Image(ctx context.Context, c tool.Caller, a ImageArgs) (ImagePart, error) {
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
