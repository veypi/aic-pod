// Package exec_procs owns command executions and output, never service business state.
package exec_procs

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const MaxLines = 1000
const DefaultExecTimeout = 30 * time.Minute
const MaxOutputBytes = 16 << 20
const MaxPreviewBytes = 128 << 10
const MaxEntries = 512
const MaxExecutionIDs = 8192
const MaxRetainedOutputBytes = 256 << 20
const CompletedTTL = time.Hour

type entryKey struct{}
type outputKey struct{}

func WithOutput(ctx context.Context, w io.Writer) context.Context {
	return context.WithValue(ctx, outputKey{}, w)
}
func Output(ctx context.Context) io.Writer { w, _ := ctx.Value(outputKey{}).(io.Writer); return w }

type Entry struct {
	ID, Command, LogPath, Owner, Digest string
	RequiredLevel                       int
	Started                             time.Time
	Timeout                             time.Duration
	pid                                 atomic.Int64
	cancel                              context.CancelFunc
	done                                chan struct{}
	mu                                  sync.Mutex
	status                              string
	completed                           time.Time
	value                               any
	fault                               *wire.Fault
	ExitCode                            int
	process                             bool
	truncated                           bool
}

func (e *Entry) Done() bool {
	select {
	case <-e.done:
		return true
	default:
		return false
	}
}
func (e *Entry) PID() int       { return int(e.pid.Load()) }
func (e *Entry) Status() string { e.mu.Lock(); defer e.mu.Unlock(); return e.status }

type Result struct {
	Command     string      `json:"command"`
	Content     string      `json:"content"`
	Lines       int         `json:"lines"`
	Truncated   bool        `json:"truncated"`
	ExitCode    int         `json:"-"`
	ProcessExit *int        `json:"exit_code,omitempty"`
	Background  bool        `json:"background"`
	ID          string      `json:"id"`
	LogPath     string      `json:"output"`
	Status      string      `json:"status"`
	Value       any         `json:"result,omitempty"`
	Error       *wire.Fault `json:"error,omitempty"`
}
type CallOptions struct {
	ID, Owner, Digest, Command, LogPath string
	RequiredLevel                       int
	Timeout                             time.Duration
	AuthorizationDeadline               time.Time
	// Check revalidates the captured authorization while the execution is alive.
	Check func(context.Context) error
	Run   func(context.Context, io.Writer) (any, error)
}
type TaskOptions struct {
	ID, Command, LogPath string
	Run                  func(context.Context, io.Writer) error
}
type Manager struct {
	mu          sync.Mutex
	execTimeout time.Duration
	tasks       map[string]*Entry
	noSandbox   atomic.Bool
	closed      bool
	epoch       string
	seen        map[string]bool
}

func NewManager(timeout time.Duration) *Manager {
	if timeout <= 0 {
		timeout = DefaultExecTimeout
	}
	return &Manager{execTimeout: timeout, tasks: map[string]*Entry{}, epoch: wire.NewID("exec_"), seen: map[string]bool{}}
}
func (m *Manager) SetExecTimeout(d time.Duration) {
	if d <= 0 {
		d = DefaultExecTimeout
	}
	m.mu.Lock()
	m.execTimeout = d
	m.mu.Unlock()
}

// StartCall creates once, or attaches to an identical retained execution. The
// wait context only controls this caller's wait. Authorization bounds the run.
func (m *Manager) StartCall(wait context.Context, o CallOptions) (*Result, error) {
	if o.Run == nil || o.ID == "" || o.LogPath == "" {
		return nil, fmt.Errorf("exec: identity, output and handler required")
	}
	if err := wait.Err(); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return nil, err
	}
	m.mu.Lock()
	m.reapLocked()
	if m.closed {
		m.mu.Unlock()
		return nil, wire.Fail("closed", "Execution service closed")
	}
	if e := m.tasks[o.ID]; e != nil {
		m.mu.Unlock()
		if e.Owner != o.Owner || e.Digest != o.Digest {
			return nil, wire.Fail("conflict", "Execution ID has different owner or invocation")
		}
		return m.await(wait, e), nil
	}
	if m.seen[o.ID] {
		m.mu.Unlock()
		return nil, wire.Fail("expired", "Execution result expired; this ID cannot be run again")
	}
	if len(m.seen) >= MaxExecutionIDs {
		m.mu.Unlock()
		return nil, wire.Fail("overloaded", "Execution identity quota reached for this runtime")
	}
	total := int64(MaxOutputBytes)
	for _, entry := range m.tasks {
		if !entry.Done() {
			total += MaxOutputBytes
		} else if info, err := os.Stat(entry.LogPath); err == nil {
			total += info.Size()
		}
	}
	if total > MaxRetainedOutputBytes {
		m.mu.Unlock()
		return nil, wire.Fail("overloaded", "Execution output storage quota reached")
	}
	if len(m.tasks) >= MaxEntries {
		m.mu.Unlock()
		return nil, wire.Fail("overloaded", "Execution retention limit")
	}
	timeout := o.Timeout
	if timeout <= 0 || timeout > m.execTimeout {
		timeout = m.execTimeout
	}
	end := time.Now().Add(timeout)
	if !o.AuthorizationDeadline.IsZero() && o.AuthorizationDeadline.Before(end) {
		end = o.AuthorizationDeadline
	}
	if !end.After(time.Now()) {
		m.mu.Unlock()
		return nil, wire.Fail("unauthorized", "Execution authorization expired")
	}
	if err := os.MkdirAll(filepath.Dir(o.LogPath), 0700); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	// Never truncate an existing execution's output, including after a restart.
	file, err := os.OpenFile(o.LogPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		m.mu.Unlock()
		return nil, wire.Fail("output_conflict", "Output exists or cannot be created; use bg_wait for an existing execution")
	}
	run, cancel := context.WithDeadline(context.Background(), end)
	e := &Entry{RequiredLevel: o.RequiredLevel, ID: o.ID, Owner: o.Owner, Digest: o.Digest, Command: o.Command, LogPath: o.LogPath, Started: time.Now(), Timeout: time.Until(end), cancel: cancel, done: make(chan struct{}), status: "running"}
	m.tasks[o.ID] = e
	m.seen[o.ID] = true
	m.mu.Unlock()
	out := &boundedWriter{file: file, remaining: MaxOutputBytes, entry: e}
	run = WithOutput(context.WithValue(run, entryKey{}, e), out)
	if o.Check != nil {
		go func() {
			tick := time.NewTicker(time.Second)
			defer tick.Stop()
			for {
				select {
				case <-run.Done():
					return
				case <-tick.C:
					if o.Check(run) != nil {
						cancel()
						return
					}
				}
			}
		}()
	}
	context.AfterFunc(run, func() {
		e.mu.Lock()
		if !e.Done() {
			e.status = "cancelling"
		}
		e.mu.Unlock()
	})
	go func() {
		var value any
		var failure error
		func() {
			defer func() {
				if recover() != nil {
					failure = wire.Fail("internal", "Command panicked; effects may have occurred")
				}
			}()
			if o.Check != nil {
				failure = o.Check(run)
			}
			if failure == nil {
				value, failure = o.Run(run, out)
			}
		}()
		if run.Err() != nil {
			failure = run.Err()
		}
		// Preserve bounded typed results, independently of the log preview.
		if raw, err := json.Marshal(value); err != nil {
			failure = err
			value = nil
		} else if len(raw) > wire.MaxMessageBytes/2 {
			failure = wire.Fail("output_limit", "Typed result exceeds limit")
			value = nil
		}
		if failure != nil {
			fmt.Fprintln(out, failure)
		}
		file.Close()
		e.mu.Lock()
		e.value = value
		e.status = "succeeded"
		if failure != nil {
			e.fault = wire.AsFault(failure)
			e.status = "failed"
			if errors.Is(failure, context.Canceled) || errors.Is(failure, context.DeadlineExceeded) {
				e.status = "cancelled"
			}
		}
		if e.process && e.ExitCode != 0 && failure == nil {
			e.status = "failed"
		}
		e.completed = time.Now()
		close(e.done)
		e.mu.Unlock()
		cancel()
	}()
	return m.await(wait, e), nil
}
func (m *Manager) await(ctx context.Context, e *Entry) *Result {
	select {
	case <-e.done:
	case <-ctx.Done():
	}
	return m.readResult(e, !e.Done())
}
func (m *Manager) Start(ctx context.Context, o StartOptions) (*Result, error) {
	res, err := m.StartCall(ctx, CallOptions{ID: o.ID, Command: o.Command, LogPath: o.LogPath, Digest: fmt.Sprintf("%q/%s", o.Exec, o.Workdir), Run: func(run context.Context, out io.Writer) (any, error) {
		code, err := m.RunProcess(run, o, out)
		if e, _ := run.Value(entryKey{}).(*Entry); e != nil {
			e.mu.Lock()
			e.process = true
			e.ExitCode = code
			e.mu.Unlock()
		}
		return nil, err
	}})
	if err == nil && res.Error != nil {
		return nil, res.Error
	}
	return res, err
}
func (m *Manager) StartTask(ctx context.Context, o TaskOptions) (*Result, error) {
	res, err := m.StartCall(ctx, CallOptions{ID: o.ID, Command: o.Command, LogPath: o.LogPath, Digest: o.Command, Run: func(run context.Context, out io.Writer) (any, error) { return nil, o.Run(run, out) }})
	if err == nil && res.Error != nil {
		return nil, res.Error
	}
	return res, err
}
func (m *Manager) RecordExit(ctx context.Context, code int) {
	if e, _ := ctx.Value(entryKey{}).(*Entry); e != nil {
		e.mu.Lock()
		e.process = true
		e.ExitCode = code
		e.mu.Unlock()
	}
}
func (m *Manager) readResult(e *Entry, bg bool) *Result {
	content, lines, truncated := readHeadLines(e.LogPath, MaxLines)
	e.mu.Lock()
	defer e.mu.Unlock()
	r := &Result{Command: e.Command, Content: content, Lines: lines, Truncated: truncated || e.truncated, ExitCode: e.ExitCode, Background: bg, ID: e.ID, LogPath: e.LogPath, Status: e.status, Value: e.value, Error: e.fault}
	if e.process && e.Done() {
		code := e.ExitCode
		r.ProcessExit = &code
	}
	return r
}
func (m *Manager) List() []*Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reapLocked()
	out := []*Entry{}
	for _, e := range m.tasks {
		if !e.Done() {
			out = append(out, e)
		}
	}
	return out
}
func (m *Manager) Wait(ctx context.Context, id string, wait time.Duration) (*Result, error) {
	e := m.Get(id)
	if e == nil {
		return nil, wire.Fail("not_found", "Execution unavailable or expired")
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	return m.await(ctx, e), nil
}
func (m *Manager) Kill(id string) error {
	e := m.Get(id)
	if e == nil || e.Done() {
		return wire.Fail("not_found", "No running execution")
	}
	e.mu.Lock()
	e.status = "cancelling"
	e.mu.Unlock()
	killEntry(e)
	return nil
}
func (m *Manager) Get(id string) *Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reapLocked()
	return m.tasks[id]
}
func (m *Manager) reapLocked() {
	now := time.Now()
	for id, e := range m.tasks {
		if e.Done() {
			e.mu.Lock()
			expired := now.Sub(e.completed) > CompletedTTL
			e.mu.Unlock()
			if expired {
				delete(m.tasks, id)
				_ = os.Remove(e.LogPath)
			}
		}
	}
}
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	entries := []*Entry{}
	for _, e := range m.tasks {
		entries = append(entries, e)
	}
	m.mu.Unlock()
	for _, e := range entries {
		if !e.Done() {
			killEntry(e)
		}
	}
	for _, e := range entries {
		select {
		case <-e.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

type boundedWriter struct {
	mu        sync.Mutex
	file      *os.File
	remaining int
	entry     *Entry
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	if n > w.remaining {
		w.entry.mu.Lock()
		w.entry.truncated = true
		w.entry.mu.Unlock()
		p = p[:w.remaining]
	}
	written, err := w.file.Write(p)
	w.remaining -= written
	if err != nil {
		return written, err
	}
	return n, nil
}
func readHeadLines(path string, maxLines int) (content string, lines int, truncated bool) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(io.LimitReader(f, MaxPreviewBytes+1))
	scanner.Buffer(make([]byte, 4096), MaxPreviewBytes+1)
	var b strings.Builder
	for lines < maxLines && scanner.Scan() {
		text := scanner.Text()
		if b.Len()+len(text)+1 > MaxPreviewBytes {
			truncated = true
			break
		}
		lines++
		b.WriteString(text)
		b.WriteByte('\n')
	}
	if scanner.Scan() || scanner.Err() != nil {
		truncated = true
	}
	if info, e := f.Stat(); e == nil && info.Size() > int64(b.Len()) {
		truncated = true
	}
	return b.String(), lines, truncated
}

func (m *Manager) Epoch() string { return m.epoch }

func (m *Manager) SetNoSandbox(value bool) { m.noSandbox.Store(value) }
