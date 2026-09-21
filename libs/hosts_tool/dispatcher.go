package hosts_tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
	"sort"
	"strings"
	"sync"
	"time"
)

type Config struct {
	ExecutionEpoch       string
	Authorize            func(context.Context, Caller, string, wire.Method) error
	MaxCalls, MaxStreams int
	Execute              func(context.Context, Caller, wire.Request, wire.Invocation, Method) (any, error)
}
type activeStream struct {
	mu     sync.Mutex
	source Stream
	caller Caller
	tool   string
	method wire.Method
	cancel context.CancelFunc
	once   sync.Once
}

func (s *activeStream) close() {
	s.once.Do(func() {
		s.cancel()
		s.mu.Lock()
		source := s.source
		s.source = nil
		s.mu.Unlock()
		if source != nil {
			_ = source.Close()
		}
	})
}

type Dispatcher struct {
	mu       sync.Mutex
	cfg      Config
	commands map[string]Command
	fs       []Method
	calls    map[string]context.CancelFunc
	streams  map[string]*activeStream
	closed   bool
	wg       sync.WaitGroup
}

func New(c Config) *Dispatcher {
	if c.MaxCalls <= 0 {
		c.MaxCalls = 128
	}
	if c.MaxStreams <= 0 {
		c.MaxStreams = 32
	}
	return &Dispatcher{cfg: c, commands: map[string]Command{}, calls: map[string]context.CancelFunc{}, streams: map[string]*activeStream{}}
}
func (d *Dispatcher) HasCommand(name string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.commands[name]
	return ok
}
func (d *Dispatcher) RegisterCommand(t Command) error {
	if !wire.ValidName(t.Name) {
		return fmt.Errorf("invalid tool name")
	}
	seen := map[string]bool{}
	for _, m := range t.Methods {
		v := m.Descriptor
		if !wire.ValidName(v.Name) || seen[v.Name] || v.Access < 1 || v.Access > 9 {
			return fmt.Errorf("invalid or duplicate method %s", v.Name)
		}
		if (v.Mode == wire.Call && m.Run == nil) || (v.Mode == wire.Stream && m.Open == nil) || (v.Mode != wire.Call && v.Mode != wire.Stream) {
			return fmt.Errorf("invalid method binding %s", v.Name)
		}
		seen[v.Name] = true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return wire.Fail("closed", "Dispatcher closed")
	}
	if _, ok := d.commands[t.Name]; ok {
		return fmt.Errorf("tool already registered: %s", t.Name)
	}
	t.Methods = append([]Method(nil), t.Methods...)
	d.commands[t.Name] = t
	return nil
}
func (d *Dispatcher) authorize(ctx context.Context, c Caller, tool string, m wire.Method) error {
	if err := c.Validate(ctx); err != nil {
		return err
	}
	if c.Scope == "fs" && tool != "fs" {
		return wire.Fail("permission_denied", "File proxy only permits fs")
	}
	if c.Level < m.Access {
		return wire.Fail("permission_denied", "Method requires a higher grant")
	}
	if d.cfg.Authorize != nil {
		return d.cfg.Authorize(ctx, c, tool, m)
	}
	return nil
}
func (d *Dispatcher) Commands(ctx context.Context, c Caller) []wire.Command {
	d.mu.Lock()
	list := make([]Command, 0, len(d.commands))
	for _, t := range d.commands {
		list = append(list, t)
	}
	d.mu.Unlock()
	out := []wire.Command{}
	for _, t := range list {
		v := wire.Command{Name: t.Name, Desc: t.Desc, Help: t.Help, RequiredLevel: t.Access, RawArgv: t.RawArgv, Methods: []wire.Method{}}
		for _, m := range t.Methods {
			if (c.AllowStreams || m.Descriptor.Mode == wire.Call) && d.authorize(ctx, c, t.Name, m.Descriptor) == nil {
				v.Methods = append(v.Methods, m.Descriptor)
			}
		}
		if len(v.Methods) > 0 {
			if v.RequiredLevel == 0 {
				v.RequiredLevel = 9
				for _, m := range v.Methods {
					if m.Access < v.RequiredLevel {
						v.RequiredLevel = m.Access
					}
				}
			}
			if v.Desc == "" {
				v.Desc = v.Name + " commands"
			}
			if v.Help == "" {
				v.Help = commandHelp(v)
			}
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
func (d *Dispatcher) lookup(in wire.Invocation) (Method, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return Method{}, wire.Fail("closed", "Dispatcher closed")
	}
	methods := d.fs
	if in.Domain == "exec" {
		methods = d.commands[in.Command].Methods
	} else if in.Domain != "fs" {
		return Method{}, wire.Fail("unsupported", "Unknown capability")
	}
	for _, m := range methods {
		if m.Descriptor.Name == in.Method {
			return m, nil
		}
	}
	return Method{}, wire.Fail("unsupported", "Unknown tool method")
}
func callKey(c Caller, id string) string {
	return c.Subject + "\x00" + c.Origin + "\x00" + c.ConnectionID + "\x00" + id
}
func (d *Dispatcher) Handle(ctx context.Context, c Caller, r wire.Request) (response wire.Response) {
	response = wire.Reply(r.Protocol, r.ID, nil, nil)
	defer func() {
		if recover() != nil {
			response = wire.Reply(r.Protocol, r.ID, nil, wire.Fail("internal", "Command handler panicked; effects may have occurred"))
		}
	}()
	if err := r.Validate(); err != nil {
		return wire.Reply(r.Protocol, r.ID, nil, err)
	}
	if err := c.Validate(ctx); err != nil {
		return wire.Reply(r.Protocol, r.ID, nil, err)
	}
	c.RequestID = r.ID
	v, err := d.handle(ctx, c, r)
	return wire.Reply(r.Protocol, r.ID, v, err)
}
func (d *Dispatcher) handle(ctx context.Context, c Caller, r wire.Request) (any, error) {
	if r.Action == "catalog" {
		return d.catalogQuery(ctx, c, r.Catalog), nil
	}
	if r.Action == "call.cancel" {
		d.mu.Lock()
		cancel := d.calls[callKey(c, r.CancelID)]
		d.mu.Unlock()
		if cancel == nil {
			return nil, wire.Fail("not_found", "No active call")
		}
		cancel()
		return map[string]bool{"cancel_requested": true}, nil
	}
	if r.Action != "call" {
		return nil, wire.Fail("unsupported", "Action is not a callable capability")
	}
	in := r.Call
	if len(r.Argv) > 0 {
		v, err := d.Translate(r.Argv)
		if err != nil {
			return nil, err
		}
		in = &v
	}
	m, err := d.lookup(*in)
	if err != nil {
		return nil, err
	}
	if m.Descriptor.Mode != wire.Call {
		return nil, wire.Fail("unsupported", "Stream methods require an RTC channel")
	}
	if m.Validate != nil {
		if err := m.Validate(in.Args); err != nil {
			return nil, err
		}
	}
	m.Descriptor.Access = m.Descriptor.RequiredLevel(in.Args)
	if m.Required != nil {
		m.Descriptor.Access = max(m.Descriptor.Access, m.Required(in.Args))
	}
	if err = d.authorize(ctx, c, in.Target(), m.Descriptor); err != nil {
		return nil, err
	}
	budget := 30 * time.Second
	if r.TimeoutMS > 0 {
		budget = time.Duration(r.TimeoutMS) * time.Millisecond
	}
	end := time.Now().Add(budget)
	if c.Deadline().Before(end) {
		end = c.Deadline()
	}
	run, cancel := context.WithDeadline(ctx, end)
	key := callKey(c, r.ID)
	d.mu.Lock()
	if d.closed || len(d.calls) >= d.cfg.MaxCalls || d.calls[key] != nil {
		d.mu.Unlock()
		cancel()
		return nil, wire.Fail("overloaded", "Call limit or duplicate active request")
	}
	d.calls[key] = cancel
	d.wg.Add(1)
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.calls, key)
		d.mu.Unlock()
		d.wg.Done()
		cancel()
	}()
	if err := d.authorize(run, c, in.Target(), m.Descriptor); err != nil {
		return nil, err
	}
	// Handlers can recheck the same mutable policy after waiting for their own locks.
	original := c
	c.Check = func(ctx context.Context) error { return d.authorize(ctx, original, in.Target(), m.Descriptor) }
	var v any
	if d.cfg.Execute != nil && in.Domain == "exec" {
		v, err = d.cfg.Execute(run, c, r, *in, m)
	} else {
		v, err = m.Run(run, c, in.Args)
	}
	if err != nil {
		fault := wire.AsFault(err)
		if fault.Details == nil && v != nil {
			fault.Details = v
		}
		return nil, fault
	}
	raw, e := json.Marshal(v)
	if e != nil {
		return nil, e
	}
	if len(raw) > wire.MaxMessageBytes/2 {
		return nil, wire.Fail("output_limit", "Result exceeds call limit; use bounded result pages; effects may have occurred")
	}
	return v, nil
}

// OpenStream is called only by an authenticated RTC attachment, never by the
// call/NATS dispatcher. Its context follows the channel, not an RPC deadline.
func (d *Dispatcher) OpenStream(ctx context.Context, c Caller, in wire.Invocation) (Stream, error) {
	if !c.AllowStreams {
		return nil, wire.Fail("unsupported", "Streams require RTC")
	}
	m, err := d.lookup(in)
	if err != nil {
		return nil, err
	}
	if m.Descriptor.Mode != wire.Stream {
		return nil, wire.Fail("invalid_argument", "Expected stream method")
	}
	m.Descriptor.Access = m.Descriptor.RequiredLevel(in.Args)
	if err := d.authorize(ctx, c, in.Target(), m.Descriptor); err != nil {
		return nil, err
	}
	run, cancel := context.WithCancel(ctx)
	id := wire.NewID("attachment_")
	s := &activeStream{caller: c, tool: in.Target(), method: m.Descriptor, cancel: cancel}
	d.mu.Lock()
	if d.closed || len(d.streams) >= d.cfg.MaxStreams {
		d.mu.Unlock()
		cancel()
		return nil, wire.Fail("overloaded", "Channel limit")
	}
	d.streams[id] = s
	d.wg.Add(1)
	d.mu.Unlock()
	defer d.wg.Done()
	original := c
	c.Check = func(ctx context.Context) error { return d.authorize(ctx, original, in.Target(), m.Descriptor) }
	source, err := openSafely(m, run, c, in.Args)
	if err != nil {
		d.dropStream(id, s)
		return nil, err
	}
	// Publish after Open; shutdown may have cancelled the reservation meanwhile.
	d.mu.Lock()
	if run.Err() != nil || d.closed {
		d.mu.Unlock()
		source.Close()
		d.dropStream(id, s)
		return nil, wire.Fail("closed", "Channel closed while opening")
	}
	s.mu.Lock()
	if run.Err() != nil {
		s.mu.Unlock()
		d.mu.Unlock()
		source.Close()
		d.dropStream(id, s)
		return nil, wire.Fail("closed", "Channel closed while opening")
	}
	s.source = source
	s.mu.Unlock()
	d.mu.Unlock()
	bound := &boundStream{Stream: source, ctx: run, check: c.Validate, close: func() { d.dropStream(id, s) }}
	context.AfterFunc(run, func() { _ = bound.Close() })
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-run.Done():
				return
			case <-ticker.C:
				if c.Validate(run) != nil {
					bound.Close()
					return
				}
			}
		}
	}()
	return bound, nil
}

type boundStream struct {
	Stream
	ctx   context.Context
	check func(context.Context) error
	close func()
}

func (s *boundStream) Send(ctx context.Context, b []byte) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if err := s.ctx.Err(); err != nil {
		return err
	}
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	return s.Stream.Send(run, b)
}
func (s *boundStream) Recv(ctx context.Context) ([]byte, error) {
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	b, err := s.Stream.Recv(run)
	if err == nil {
		err = s.check(ctx)
	}
	return b, err
}
func (s *boundStream) Close() error { s.close(); return nil }

func (d *Dispatcher) dropStream(id string, s *activeStream) {
	d.mu.Lock()
	if d.streams[id] == s {
		delete(d.streams, id)
	}
	d.mu.Unlock()
	s.close()
}
func (d *Dispatcher) Disconnect(c Caller) {
	prefix := callKey(c, "")
	d.mu.Lock()
	calls := []context.CancelFunc{}
	streams := map[string]*activeStream{}
	for k, v := range d.calls {
		if strings.HasPrefix(k, prefix) {
			calls = append(calls, v)
		}
	}
	for id, s := range d.streams {
		if s.caller.Subject == c.Subject && s.caller.ConnectionID == c.ConnectionID {
			streams[id] = s
		}
	}
	d.mu.Unlock()
	for _, c := range calls {
		c()
	}
	for id, s := range streams {
		d.dropStream(id, s)
	}
}
func (d *Dispatcher) Close(ctx context.Context) error {
	d.mu.Lock()
	d.closed = true
	for _, cancel := range d.calls {
		cancel()
	}
	streams := d.streams
	d.streams = map[string]*activeStream{}
	list := []Command{}
	for _, t := range d.commands {
		list = append(list, t)
	}
	d.mu.Unlock()
	for _, s := range streams {
		s.close()
	}
	done := make(chan struct{})
	go func() { d.wg.Wait(); close(done) }()
	var failures []error
	select {
	case <-ctx.Done():
		failures = append(failures, ctx.Err())
	case <-done:
	}
	// Teardown every tool even when an active handler misses its shutdown budget.
	for _, t := range list {
		if t.Close != nil {
			if err := t.Close(); err != nil {
				failures = append(failures, err)
			}
		}
	}
	return errors.Join(failures...)
}

func openSafely(m Method, ctx context.Context, c Caller, args json.RawMessage) (source Stream, err error) {
	defer func() {
		if recover() != nil {
			err = wire.Fail("internal", "Command channel opener panicked")
		}
	}()
	source, err = m.Open(ctx, c, args)
	if err == nil && source == nil {
		err = wire.Fail("internal", "Command returned no channel")
	}
	return
}

// RegisterFS binds the built-in filesystem, outside the exec command namespace.
func (d *Dispatcher) RegisterFS(methods ...Method) error {
	for _, m := range methods {
		if !wire.ValidName(m.Descriptor.Name) || m.Descriptor.Access < 1 || m.Run == nil {
			return fmt.Errorf("invalid fs method")
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.fs != nil {
		return fmt.Errorf("filesystem already registered or dispatcher closed")
	}
	seen := map[string]bool{}
	for _, m := range methods {
		if seen[m.Descriptor.Name] {
			return fmt.Errorf("duplicate fs method")
		}
		seen[m.Descriptor.Name] = true
	}
	d.fs = append([]Method{}, methods...)
	return nil
}
func (d *Dispatcher) Catalog(ctx context.Context, c Caller) wire.Catalog {
	out := wire.Catalog{FS: []wire.Method{}}
	out.Exec.Commands = d.Commands(ctx, c)
	out.Exec.Epoch = d.cfg.ExecutionEpoch
	d.mu.Lock()
	methods := append([]Method{}, d.fs...)
	d.mu.Unlock()
	for _, m := range methods {
		if (c.AllowStreams || m.Descriptor.Mode == wire.Call) && d.authorize(ctx, c, "fs", m.Descriptor) == nil {
			out.FS = append(out.FS, m.Descriptor)
		}
	}
	return out
}

func commandHelp(c wire.Command) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s <method> [--field value]\nNested fields use JSON.\n", c.Name)
	for _, m := range c.Methods {
		if m.Mode != wire.Call {
			continue
		}
		fmt.Fprintf(&b, "\n%s %s", c.Name, m.Name)
		if m.CLI != "" && m.CLI != m.Name {
			fmt.Fprintf(&b, " (alias: %s)", m.CLI)
		}
		if len(m.Positionals) > 0 {
			fmt.Fprintf(&b, "; positional: %s", strings.Join(m.Positionals, ", "))
		}
		fmt.Fprintf(&b, "\n%s\ninput: %s\n", m.Description, m.Input)
	}
	return b.String()
}

func (d *Dispatcher) catalogQuery(ctx context.Context, c Caller, q *wire.CatalogQuery) wire.Catalog {
	catalog := d.Catalog(ctx, c)
	if q != nil {
		if q.Domain == "fs" {
			catalog.Exec.Commands = []wire.Command{}
		} else {
			catalog.FS = []wire.Method{}
			commands := []wire.Command{}
			for _, command := range catalog.Exec.Commands {
				if command.Name == q.Command {
					commands = append(commands, command)
				}
			}
			catalog.Exec.Commands = commands
		}
		return catalog
	}
	// The index retains method names and access. Load schemas per capability to
	// stay below RTC message limits as the command registry grows.
	for i := range catalog.FS {
		catalog.FS[i].Input = nil
		catalog.FS[i].Output = nil
	}
	for i := range catalog.Exec.Commands {
		command := &catalog.Exec.Commands[i]
		command.Help = ""
		for j := range command.Methods {
			command.Methods[j].Input = nil
			command.Methods[j].Output = nil
		}
	}
	return catalog
}
