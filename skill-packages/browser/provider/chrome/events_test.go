package chrome

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestEventPressurePreservesLifecycleAndACKsDroppedFrames(t *testing.T) {
	c := &Conn{events: make(chan Event), eventWake: make(chan struct{}, 1), frameAcks: make(chan Event, 512), done: make(chan struct{})}
	first := Event{Method: "Target.targetCreated", Params: json.RawMessage(`{"targetId":"first"}`)}
	if err := c.enqueueEvent(first); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2048; i++ {
		if err := c.enqueueEvent(Event{Method: "Runtime.consoleAPICalled", Params: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if err := c.enqueueEvent(Event{Method: "Page.screencastFrame", Session: "page", Params: json.RawMessage(`{"sessionId":7,"data":"old frame"}`)}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 1024; i++ {
		_ = c.enqueueEvent(Event{Method: "Network.responseReceived", Params: json.RawMessage(`{}`)})
	}
	if len(c.frameAcks) != 3 {
		t.Fatal("dropped frames lost their ACKs")
	}
	last := Event{Method: "Target.targetDestroyed", Params: json.RawMessage(`{"targetId":"last"}`)}
	if err := c.enqueueEvent(last); err != nil {
		t.Fatal(err)
	}
	if len(c.eventQueue) > maxQueuedEvents || c.eventBytes > maxEventBytes {
		t.Fatal("unbounded event storage")
	}
	go c.dispatchEvents()
	defer close(c.done)
	var lifecycle []string
	deadline := time.After(time.Second)
	for {
		select {
		case e := <-c.Events():
			if !disposableEvent(e) {
				lifecycle = append(lifecycle, e.Method)
			}
			if e.Method == last.Method {
				if len(lifecycle) != 2 || lifecycle[0] != first.Method {
					t.Fatal("lost or reordered lifecycle events", lifecycle)
				}
				return
			}
		case <-deadline:
			t.Fatal("events stopped draining")
		}
	}
}

func TestFrameACKDoesNotNeedAnEventConsumer(t *testing.T) {
	read, peerWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	peerRead, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer peerWrite.Close()
	defer peerRead.Close()
	c := &Conn{read: read, write: write, closing: true, pending: map[int64]chan message{}, writeGate: make(chan struct{}, 1), events: make(chan Event), eventWake: make(chan struct{}, 1), frameAcks: make(chan Event, 512), done: make(chan struct{})}
	defer c.fail(context.Canceled)
	go c.loop()
	go c.dispatchEvents()
	go c.ackFrames()
	seen := make(chan string, 2)
	go func() {
		r := bufio.NewReader(peerRead)
		for {
			raw, err := r.ReadBytes(0)
			if err != nil {
				return
			}
			var request struct {
				ID      int64           `json:"id"`
				Method  string          `json:"method"`
				Session string          `json:"sessionId"`
				Params  json.RawMessage `json:"params"`
			}
			if json.Unmarshal(raw[:len(raw)-1], &request) != nil {
				return
			}
			if request.Method == "Page.screencastFrameAck" && (request.Session != "page" || string(request.Params) != `{"sessionId":7}`) {
				seen <- "bad ACK"
			} else {
				seen <- request.Method
			}
			reply, _ := json.Marshal(map[string]any{"id": request.ID, "result": map[string]any{}})
			if _, err = peerWrite.Write(append(reply, 0)); err != nil {
				return
			}
		}
	}()
	if err := c.enqueueEvent(Event{Method: "Page.screencastFrame", Session: "page", Params: json.RawMessage(`{"sessionId":7}`)}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Call(ctx, "", "Browser.getVersion", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	methods := map[string]bool{}
	for len(methods) < 2 {
		select {
		case method := <-seen:
			methods[method] = true
		case <-ctx.Done():
			t.Fatal("ACK or command reply stalled")
		}
	}
	if !methods["Page.screencastFrameAck"] || !methods["Browser.getVersion"] {
		t.Fatal(methods)
	}
}
