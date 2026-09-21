package browser

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	tool "github.com/veypi/aic-pod/libs/hosts_tool"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
	"io"
	"strings"
	"sync"
	"time"
)

// The browser viewer alone understands this frame packet: a four-byte JSON
// header length, a tool-owned header, and raw JPEG bytes. It is not hosts_tools.
const frameChunkBytes = 24 << 10

type frameChunk struct {
	Metadata json.RawMessage `json:"metadata,omitempty"`
	Final    bool            `json:"final"`
}

func encodeFrameChunk(header frameChunk, data []byte) []byte {
	raw, _ := json.Marshal(header)
	packet := make([]byte, 4+len(raw)+len(data))
	binary.BigEndian.PutUint32(packet, uint32(len(raw)))
	copy(packet[4:], raw)
	copy(packet[4+len(raw):], data)
	return packet
}

type frameData struct {
	data []byte
	meta json.RawMessage
	seq  uint64
}
type frameStream struct {
	p       *page
	ctx     context.Context
	cancel  context.CancelFunc
	frames  chan frameData
	current frameData
	offset  int
	once    sync.Once
	mu      sync.Mutex
	latest  uint64
}

func (f *frameStream) offer(frame frameData) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if frame.seq <= f.latest || f.ctx.Err() != nil {
		return
	}
	f.latest = frame.seq
	select {
	case f.frames <- frame:
	default:
		select {
		case <-f.frames:
		default:
		}
		select {
		case f.frames <- frame:
		default:
		}
	}
}
func (f *frameStream) Recv(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := f.ctx.Err(); err != nil {
		return nil, err
	}
	if f.current.data == nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-f.ctx.Done():
			return nil, io.EOF
		case f.current = <-f.frames:
		}
		f.offset = 0
	}
	end := min(f.offset+frameChunkBytes, len(f.current.data))
	v := frameChunk{Final: end == len(f.current.data)}
	data := f.current.data[f.offset:end]
	if f.offset == 0 {
		v.Metadata = f.current.meta
	}
	f.offset = end
	if v.Final {
		f.current = frameData{}
	}
	return encodeFrameChunk(v, data), nil
}
func (f *frameStream) Send(context.Context, []byte) error {
	return wire.Fail("unsupported", "Read only stream")
}
func (f *frameStream) Close() error {
	f.once.Do(func() {
		f.cancel()
		f.p.mu.Lock()
		delete(f.p.viewers, f)
		last := len(f.p.viewers) == 0
		f.p.mu.Unlock()
		if last {
			go func() {
				f.p.castMu.Lock()
				defer f.p.castMu.Unlock()
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				f.p.mu.Lock()
				if len(f.p.viewers) != 0 {
					f.p.mu.Unlock()
					return
				}
				f.p.casting = false
				f.p.mu.Unlock()
				_ = f.p.call(ctx, "Page.stopScreencast", map[string]any{}, nil)
			}()
		}
	})
	return nil
}
func (s *Service) Frames(ctx context.Context, c tool.Caller, a PageArgs) (tool.Stream, error) {
	p, err := s.get(c, a.PageID)
	if err != nil {
		return nil, err
	}
	p.castMu.Lock()
	defer p.castMu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	f := &frameStream{p: p, ctx: ctx, cancel: cancel, frames: make(chan frameData, 1)}
	p.mu.Lock()
	if len(p.viewers) >= 8 {
		p.mu.Unlock()
		cancel()
		return nil, wire.Fail("overloaded", "Viewer limit reached")
	}
	start := !p.casting
	if start {
		p.frameTimestamp = 0
	}
	p.casting = true
	p.viewers[f] = true
	// Reserve the initial screenshot's position before starting either source.
	// A late screenshot may never replace a newer compositor frame.
	p.frameSeq++
	initialSeq, info := p.frameSeq, p.info
	p.mu.Unlock()
	if start {
		if err = p.call(ctx, "Page.startScreencast", map[string]any{"format": "jpeg", "quality": 70, "maxWidth": info.Width, "maxHeight": info.Height, "everyNthFrame": 1}, nil); err != nil {
			f.Close()
			return nil, err
		}
	}
	// One refresh supplies static pages and newly joined viewers without screenshot polling.
	go func() {
		refresh, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		var img struct {
			Data string `json:"data"`
		}
		if p.call(refresh, "Page.captureScreenshot", map[string]any{"format": "jpeg", "quality": 70, "captureBeyondViewport": false}, &img) == nil {
			data, e := base64.StdEncoding.DecodeString(img.Data)
			if e == nil && len(data) <= 4<<20 && p.snapshot().Document == info.Document {
				f.offer(makeFrame(data, info, initialSeq))
			}
		}
	}()
	return f, nil
}

func makeFrame(data []byte, info PageInfo, seq uint64) frameData {
	meta, _ := json.Marshal(map[string]any{"frame_id": wire.NewID("frame_"), "frame_seq": seq, "page_id": info.ID, "document_id": info.Document, "revision": info.Revision, "width": info.Width, "height": info.Height, "media_type": "image/jpeg", "size": len(data)})
	return frameData{data: data, meta: meta, seq: seq}
}

func (s *Service) frameEvent(p *page, raw json.RawMessage) {
	var event struct {
		Data     string `json:"data"`
		ID       int    `json:"sessionId"`
		Metadata struct {
			Timestamp float64 `json:"timestamp"`
		} `json:"metadata"`
	}
	if json.Unmarshal(raw, &event) != nil {
		return
	}
	defer func() {
		ctx, cancel := context.WithTimeout(s.ctx, time.Second)
		defer cancel()
		_ = p.call(ctx, "Page.screencastFrameAck", map[string]any{"sessionId": event.ID}, nil)
	}()
	if len(event.Data) > 6<<20 {
		return
	}
	data, err := base64.StdEncoding.DecodeString(event.Data)
	if err != nil || len(data) > 4<<20 {
		return
	}
	p.deliverFrame(data, event.Metadata.Timestamp)
}

// Screencast encodes asynchronously. Use Chrome's capture timestamp to reject
// late images as well as the sequence fence used for the initial screenshot.
func (p *page) deliverFrame(data []byte, timestamp float64) {
	p.mu.Lock()
	if !p.casting || p.closed || (timestamp > 0 && timestamp <= p.frameTimestamp) {
		p.mu.Unlock()
		return
	}
	if timestamp > 0 {
		p.frameTimestamp = timestamp
	}
	p.frameSeq++
	frame := makeFrame(data, p.info, p.frameSeq)
	viewers := []*frameStream{}
	for v := range p.viewers {
		viewers = append(viewers, v)
	}
	p.mu.Unlock()
	for _, v := range viewers {
		v.offer(frame)
	}
}

const inputIdleTimeout = 10 * time.Second

// A lease is private browser state, acquired by real input rather than a call.
// Opening an input channel alone does not reserve the page.
type lease struct {
	connection string
	input      *inputStream
	expires    time.Time       // protected by page.mu
	keys       map[string]bool // protected by input.mu
	buttons    int
}

type inputStream struct {
	queueMu     sync.Mutex
	queue       []inputEvent
	plannedKeys map[string]bool
	wake        chan struct{}
	err         error
	p           *page
	l           *lease
	caller      tool.Caller
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex
	seq         uint64
	once        sync.Once
	done        chan struct{}
}

func (s *Service) Input(ctx context.Context, c tool.Caller, a PageArgs) (tool.Stream, error) {
	p, err := s.get(c, a.PageID)
	if err != nil {
		return nil, err
	}
	if err = c.Validate(ctx); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	v := &inputStream{p: p, caller: c, ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1), plannedKeys: map[string]bool{}, done: make(chan struct{})}
	v.l = &lease{connection: c.ConnectionID, input: v, keys: map[string]bool{}}
	p.mu.Lock()
	if p.closed || len(p.inputs) >= 8 {
		p.mu.Unlock()
		cancel()
		return nil, wire.Fail("closed", "Page closed or input channel limit reached")
	}
	if p.inputs == nil {
		p.inputs = map[*inputStream]bool{}
	}
	p.inputs[v] = true
	p.mu.Unlock()
	go v.run()
	return v, nil
}
func (i *inputStream) Recv(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-i.ctx.Done():
		i.queueMu.Lock()
		err := i.err
		i.queueMu.Unlock()
		if err != nil {
			return nil, err
		}
		return nil, io.EOF
	}
}
func (i *inputStream) Close() error {
	i.once.Do(func() {
		i.cancel()
		i.releaseControl(true, false)
		i.p.mu.Lock()
		delete(i.p.inputs, i)
		i.p.mu.Unlock()
		close(i.done)
	})
	return nil
}

// Release pressed state before making the page available to automation. The
// channel survives idle expiry; new input arriving during cleanup stays queued.
func (i *inputStream) releaseControl(discard, idleOnly bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.queueMu.Lock()
	p, l := i.p, i.l
	p.mu.Lock()
	if p.lease != l || (idleOnly && (len(i.queue) > 0 || time.Now().Before(l.expires))) {
		p.mu.Unlock()
		i.queueMu.Unlock()
		return
	}
	if discard {
		i.queue = nil
	}
	if len(i.queue) == 0 {
		i.plannedKeys = map[string]bool{}
	}
	p.mu.Unlock()
	i.queueMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for key := range l.keys {
		_ = p.call(ctx, "Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": key}, nil)
	}
	if l.buttons != 0 {
		for _, button := range []string{"left", "middle", "right"} {
			_ = p.call(ctx, "Input.dispatchMouseEvent", map[string]any{"type": "mouseReleased", "x": 0, "y": 0, "button": button, "buttons": 0, "clickCount": 1}, nil)
		}
	}
	l.keys, l.buttons = map[string]bool{}, 0
	i.queueMu.Lock()
	p.mu.Lock()
	if p.lease == l && (discard || len(i.queue) == 0) {
		p.lease = nil
		p.controlEpoch++
	}
	p.mu.Unlock()
	i.queueMu.Unlock()
}

type inputEvent struct {
	Document  string  `json:"-"`
	Type      string  `json:"type"`
	X         float64 `json:"x,omitempty"`
	Y         float64 `json:"y,omitempty"`
	Button    string  `json:"button,omitempty"`
	DeltaX    float64 `json:"delta_x,omitempty"`
	DeltaY    float64 `json:"delta_y,omitempty"`
	Key       string  `json:"key,omitempty"`
	Code      string  `json:"code,omitempty"`
	Text      string  `json:"text,omitempty"`
	Modifiers int     `json:"modifiers,omitempty"`
}

type inputBatch struct {
	Seq      uint64       `json:"seq"`
	Document string       `json:"document_id"`
	Events   []inputEvent `json:"events"`
}

// Validate the whole batch before dispatching anything. Keyboard state is
// simulated so a malformed tail cannot leave a partially pressed chord.
func validateInput(batch inputBatch, width, height int, held map[string]bool) error {
	if batch.Seq == 0 || batch.Document == "" || len(batch.Events) == 0 || len(batch.Events) > 32 {
		return errArg("Expected document_id, seq and 1..32 input events")
	}
	keys := make(map[string]bool, len(held))
	for key, down := range held {
		keys[key] = down
	}
	for _, e := range batch.Events {
		if e.Modifiers < 0 || e.Modifiers > 15 {
			return errArg("Invalid modifiers")
		}
		switch e.Type {
		case "reset":
			keys = map[string]bool{}
		case "pointer.move", "pointer.down", "pointer.up", "wheel":
			if e.X < 0 || e.Y < 0 || e.X >= float64(width) || e.Y >= float64(height) {
				return errArg("Input outside viewport")
			}
			if e.Button != "" && e.Button != "none" && e.Button != "left" && e.Button != "middle" && e.Button != "right" {
				return errArg("Invalid button")
			}
			if (e.Type == "pointer.down" || e.Type == "pointer.up") && (e.Button == "" || e.Button == "none") {
				return errArg("Invalid button")
			}
			if e.Type == "wheel" && (e.DeltaX < -100000 || e.DeltaX > 100000 || e.DeltaY < -100000 || e.DeltaY > 100000) {
				return errArg("Wheel delta exceeds limit")
			}
		case "key.down", "key.up":
			if e.Key == "" || len(e.Key) > 64 || len(e.Code) > 64 {
				return errArg("Invalid key")
			}
			if e.Type == "key.up" {
				delete(keys, e.Key)
			} else {
				keys[e.Key] = true
			}
			if len(keys) > 32 {
				return errArg("Too many held keys")
			}
		case "text":
			if len(e.Text) > 16<<10 {
				return errArg("Text exceeds limit")
			}
		default:
			return errArg("Unknown input event")
		}
	}
	return nil
}

func continuousInput(e inputEvent) bool {
	return e.Type == "pointer.move" || e.Type == "wheel"
}

func coalesceInput(events []inputEvent) []inputEvent {
	result := make([]inputEvent, 0, len(events))
	for _, e := range events {
		if continuousInput(e) {
			for n := len(result) - 1; n >= 0; n-- {
				old := result[n]
				if !continuousInput(old) || old.Document != e.Document || old.Modifiers != e.Modifiers {
					break
				}
				if old.Type == e.Type {
					result = append(result[:n], result[n+1:]...)
					break
				}
			}
		}
		result = append(result, e)
	}
	return result
}

// Send accepts a tool-private input batch without waiting for CDP. Coalescing
// and ordering belong to this tool, not the RTC adapter's generic byte queue.
func (i *inputStream) Send(ctx context.Context, raw []byte) error {
	i.queueMu.Lock()
	defer i.queueMu.Unlock()
	if err := i.ctx.Err(); err != nil {
		return err
	}
	if err := i.caller.Validate(ctx); err != nil {
		return err
	}
	var batch inputBatch
	if err := wire.Decode(raw, &batch); err != nil {
		return err
	}
	p := i.p
	p.mu.Lock()
	stale := p.closed || batch.Seq <= i.seq || batch.Document != p.info.Document
	width, height := p.info.Width, p.info.Height
	p.mu.Unlock()
	if stale {
		return wire.Fail("stale_input", "Input stream, sequence or document changed")
	}
	if err := validateInput(batch, width, height, i.plannedKeys); err != nil {
		return err
	}
	for n := range batch.Events {
		batch.Events[n].Document = batch.Document
	}
	queue := coalesceInput(append(append([]inputEvent(nil), i.queue...), batch.Events...))
	if len(queue) > 128 {
		return wire.Fail("overloaded", "Browser input queue full")
	}
	active := false
	for _, e := range batch.Events {
		active = active || e.Type != "reset"
	}
	p.mu.Lock()
	if p.closed || batch.Document != p.info.Document {
		p.mu.Unlock()
		return wire.Fail("stale_input", "Page or document changed")
	}
	if active && p.lease != nil && p.lease != i.l {
		p.mu.Unlock()
		return wire.Fail("control_busy", "Page is receiving input from another viewer")
	}
	if active {
		if p.lease == nil {
			p.lease = i.l
			p.controlEpoch++
		}
		i.l.expires = time.Now().Add(inputIdleTimeout)
	}
	p.mu.Unlock()
	i.queue = queue
	i.seq = batch.Seq
	for _, e := range batch.Events {
		if e.Type == "reset" {
			i.plannedKeys = map[string]bool{}
		}
		if e.Type == "key.down" {
			i.plannedKeys[e.Key] = true
		}
		if e.Type == "key.up" {
			delete(i.plannedKeys, e.Key)
		}
	}
	select {
	case i.wake <- struct{}{}:
	default:
	}
	return nil
}
func (i *inputStream) run() {
	defer i.Close()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-i.ctx.Done():
			return
		case <-ticker.C:
			i.releaseControl(false, true)
			continue
		case <-i.wake:
		}
		for {
			worked, err := i.dispatchNext()
			if !worked && err == nil {
				break
			}
			if err != nil {
				i.queueMu.Lock()
				i.err = err
				i.queue = nil
				i.queueMu.Unlock()
				return
			}
		}
	}
}

// Wait for the page before choosing input. While an AI action holds the gate,
// newer continuous events can still replace pending ones, including reversals.
func (i *inputStream) dispatchNext() (bool, error) {
	i.queueMu.Lock()
	empty := len(i.queue) == 0
	i.queueMu.Unlock()
	if empty {
		return false, nil
	}
	select {
	case i.p.gate <- struct{}{}:
		defer func() { <-i.p.gate }()
	case <-i.ctx.Done():
		return false, i.ctx.Err()
	}
	i.queueMu.Lock()
	if len(i.queue) == 0 {
		i.queueMu.Unlock()
		return false, nil
	}
	e := i.queue[0]
	i.queue = i.queue[1:]
	i.queueMu.Unlock()
	return true, i.execute(e)
}

func (i *inputStream) execute(e inputEvent) error {
	if e.Type == "reset" {
		i.releaseControl(false, false)
		return nil
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := i.ctx.Err(); err != nil {
		return err
	}
	if err := i.caller.Validate(i.ctx); err != nil {
		return err
	}
	p := i.p
	p.mu.Lock()
	stale := p.closed || p.lease != i.l || e.Document != p.info.Document
	if !stale {
		p.info.Revision++
	}
	p.mu.Unlock()
	if stale {
		return wire.Fail("stale_input", "Input stream or document changed")
	}
	return i.dispatch(i.ctx, e)
}

func (i *inputStream) dispatch(ctx context.Context, e inputEvent) error {
	p := i.p
	switch e.Type {
	case "pointer.move", "pointer.down", "pointer.up", "wheel":
		kind := "mouseMoved"
		button := e.Button
		if button == "" {
			button = "none"
		}
		bit := map[string]int{"left": 1, "right": 2, "middle": 4}[button]
		switch e.Type {
		case "pointer.down":
			kind = "mousePressed"
			i.l.buttons |= bit
		case "pointer.up":
			kind = "mouseReleased"
			i.l.buttons &^= bit
		case "wheel":
			kind = "mouseWheel"
		}
		params := map[string]any{"type": kind, "x": e.X, "y": e.Y, "button": button, "buttons": i.l.buttons, "modifiers": e.Modifiers}
		if kind == "mouseWheel" {
			params["deltaX"] = e.DeltaX
			params["deltaY"] = e.DeltaY
		} else if kind != "mouseMoved" {
			params["clickCount"] = 1
		}
		return p.call(ctx, "Input.dispatchMouseEvent", params, nil)
	case "key.down", "key.up":
		kind := "keyDown"
		if e.Type == "key.up" {
			kind = "keyUp"
			delete(i.l.keys, e.Key)
		} else {
			i.l.keys[e.Key] = true
		}
		params := map[string]any{"type": kind, "key": e.Key, "code": e.Code, "modifiers": e.Modifiers}
		code := map[string]int{"Enter": 13, "Tab": 9, "Escape": 27, "Backspace": 8, "Delete": 46, "ArrowLeft": 37, "ArrowRight": 39, "ArrowUp": 38, "ArrowDown": 40, "Home": 36, "End": 35, "PageUp": 33, "PageDown": 34, " ": 32}[e.Key]
		if code == 0 && len(e.Key) == 1 {
			code = int(strings.ToUpper(e.Key)[0])
		}
		params["windowsVirtualKeyCode"] = code
		if kind == "keyDown" && e.Modifiers&6 != 0 {
			command := map[string]string{"a": "selectAll", "z": "undo", "y": "redo"}[strings.ToLower(e.Key)]
			if command == "undo" && e.Modifiers&8 != 0 {
				command = "redo"
			}
			if command != "" {
				params["commands"] = []string{command}
			}
		}
		return p.call(ctx, "Input.dispatchKeyEvent", params, nil)
	case "text":
		return p.call(ctx, "Input.insertText", map[string]any{"text": e.Text}, nil)
	}
	return errArg("Unknown input event")
}
