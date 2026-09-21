// Package chrome launches a dedicated Chrome and speaks CDP over inherited pipes.
package chrome

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"
)

const maxPacket = 16 << 20

type Event struct {
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Session string          `json:"sessionId"`
}
type message struct {
	Event
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}
type Conn struct {
	cmd         *exec.Cmd
	read, write *os.File
	mu          sync.Mutex
	writeGate   chan struct{}
	next        int64
	pending     map[int64]chan message
	events      chan Event
	eventQueue  []Event // guarded by mu; replies never wait for event consumers
	eventBytes  int
	eventWake   chan struct{}
	frameAcks   chan Event
	done        chan struct{}
	processDone chan struct{}
	once        sync.Once
	closeOnce   sync.Once
	closing     bool
	err         error
	identity    *identity // Immutable after startup; published under mu.
}

func start(ctx context.Context, path, profile string, args ...string) (*Conn, error) {
	if err := os.MkdirAll(profile, 0700); err != nil {
		return nil, err
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, err
	}
	argv := []string{"--headless=new", "--remote-debugging-pipe", "--user-data-dir=" + profile, "--no-first-run", "--no-default-browser-check", "--disable-background-networking", "--disable-component-update", "--disable-sync", "--mute-audio", "--disable-features=Translate,ElasticOverscroll,UACHOverrideBlank", "--disable-blink-features=AutomationControlled", "--no-startup-window"}
	if runtime.GOOS == "darwin" {
		// Chrome reads macOS rubber banding from NSUserDefaults, independently
		// of ElasticOverscroll. Override this child process's argument domain;
		// never change the user's system or personal Chrome preferences.
		argv = append(argv, "-NSScrollViewRubberbanding", "NO")
	}
	argv = append(argv, args...)
	cmd := exec.Command(path, argv...)
	cmd.Stderr = io.Discard
	if err = configure(cmd, inR, outW); err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, err
	}
	inR.Close()
	outW.Close()
	c := &Conn{writeGate: make(chan struct{}, 1), cmd: cmd, read: outR, write: inW, pending: map[int64]chan message{}, events: make(chan Event), eventWake: make(chan struct{}, 1), frameAcks: make(chan Event, 512), done: make(chan struct{}), processDone: make(chan struct{})}
	go c.dispatchEvents()
	go c.ackFrames()
	go c.loop()
	go func() {
		err := cmd.Wait()
		close(c.processDone)
		if err == nil {
			err = io.EOF
		}
		c.fail(err)
	}()
	var version map[string]any
	if err = c.Call(ctx, "", "Browser.getVersion", map[string]any{}, &version); err != nil {
		c.Close()
		return nil, fmt.Errorf("Chrome startup: %w", err)
	}
	return c, nil
}
func (c *Conn) Events() <-chan Event  { return c.events }
func (c *Conn) Done() <-chan struct{} { return c.done }
func (c *Conn) loop() {
	r := bufio.NewReaderSize(c.read, 64<<10)
	for {
		var packet []byte
		for {
			part, err := r.ReadSlice(0)
			if len(packet)+len(part) > maxPacket {
				c.fail(fmt.Errorf("CDP packet exceeds limit"))
				return
			}
			packet = append(packet, part...)
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if err != nil {
				c.fail(err)
				return
			}
			break
		}
		var msg message
		if json.Unmarshal(packet[:len(packet)-1], &msg) != nil {
			c.fail(fmt.Errorf("invalid CDP packet"))
			return
		}
		if msg.ID != 0 {
			c.mu.Lock()
			ch := c.pending[msg.ID]
			c.mu.Unlock()
			if ch != nil {
				ch <- msg
			}
		} else {
			if c.prepareAttachment(msg.Event) {
				continue
			}
			if err := c.enqueueEvent(msg.Event); err != nil {
				c.fail(err)
				return
			}
		}
	}
}
func (c *Conn) fail(err error) {
	c.once.Do(func() {
		c.mu.Lock()
		c.err = err
		c.eventQueue = nil
		c.eventBytes = 0
		closing := c.closing
		c.mu.Unlock()
		close(c.done)
		c.read.Close()
		c.write.Close()
		if !closing {
			kill(c.cmd)
		}
	})
}
func (c *Conn) Call(ctx context.Context, session, method string, params, result any) error {
	return c.call(ctx, session, method, params, result, nil)
}

// queueCall sends in caller order but lets a later command unblock the reply.
// Service worker startup requires this: renderer commands cannot complete until
// Runtime.runIfWaitingForDebugger has also reached the browser process.
func (c *Conn) queueCall(ctx context.Context, session, method string, params, result any) func() error {
	sent, finished := make(chan struct{}), make(chan error, 1)
	go func() { finished <- c.call(ctx, session, method, params, result, func() { close(sent) }) }()
	select {
	case <-sent:
		return func() error { return <-finished }
	case err := <-finished:
		return func() error { return err }
	}
}

func (c *Conn) call(ctx context.Context, session, method string, params, result any, sent func()) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return err
	}
	c.next++
	id := c.next
	ch := make(chan message, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	raw, err := json.Marshal(map[string]any{"id": id, "sessionId": session, "method": method, "params": params})
	if err != nil {
		return err
	}
	if session == "" {
		raw, err = json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	}
	if err != nil {
		return err
	}
	select {
	case c.writeGate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return io.EOF
	}
	if err := ctx.Err(); err != nil {
		<-c.writeGate
		return err
	}
	finished := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { c.fail(ctx.Err()); close(finished) })
	_, err = c.write.Write(append(raw, 0))
	if !stop() {
		<-finished
	}
	<-c.writeGate
	if err != nil {
		c.fail(err)
		return err
	}
	if sent != nil {
		sent()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		c.mu.Lock()
		err := c.err
		c.mu.Unlock()
		return err
	case msg := <-ch:
		if msg.Error != nil {
			return fmt.Errorf("CDP %s: %s", method, msg.Error.Message)
		}
		if result != nil {
			return json.Unmarshal(msg.Result, result)
		}
		return nil
	}
}
func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closing = true
		c.mu.Unlock()
		// Let Chrome flush cookies and profile databases before terminating it.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = c.Call(ctx, "", "Browser.close", map[string]any{}, nil)
		cancel()
		select {
		case <-c.processDone:
		case <-time.After(3 * time.Second):
			kill(c.cmd)
		}
		c.fail(io.EOF)
	})
	select {
	case <-c.processDone:
	case <-time.After(2 * time.Second):
		return fmt.Errorf("Chrome process did not exit")
	}
	return nil
}
