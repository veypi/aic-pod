package chrome

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const maxQueuedEvents = 512
const maxEventBytes = 32 << 20

func disposableEvent(e Event) bool {
	return e.Method == "Page.screencastFrame" || strings.HasPrefix(e.Method, "Network.") ||
		e.Method == "Runtime.consoleAPICalled" || e.Method == "Runtime.exceptionThrown"
}

// Keep delivery order while shedding old telemetry/frames under pressure.
// Lifecycle events are never silently discarded. A critical-only overflow is
// still an explicit failure rather than an unbounded queue or lost page state.
func (c *Conn) enqueueEvent(e Event) error {
	if e.Method == "Page.screencastFrame" {
		var frame struct {
			ID int `json:"sessionId"`
		}
		if json.Unmarshal(e.Params, &frame) == nil {
			// Queue only the tiny ACK, never a second copy of the image payload.
			raw, _ := json.Marshal(map[string]int{"sessionId": frame.ID})
			select {
			case c.frameAcks <- Event{Session: e.Session, Params: raw}:
			case <-c.done:
				return nil
			default:
				return fmt.Errorf("CDP frame acknowledgements stalled")
			}
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	for len(c.eventQueue) >= maxQueuedEvents || c.eventBytes+len(e.Params) > maxEventBytes {
		drop := -1
		for i, old := range c.eventQueue {
			if disposableEvent(old) {
				drop = i
				break
			}
		}
		if drop < 0 {
			if disposableEvent(e) {
				return nil
			}
			return fmt.Errorf("CDP lifecycle event queue overflow")
		}
		c.eventBytes -= len(c.eventQueue[drop].Params)
		copy(c.eventQueue[drop:], c.eventQueue[drop+1:])
		c.eventQueue[len(c.eventQueue)-1] = Event{}
		c.eventQueue = c.eventQueue[:len(c.eventQueue)-1]
	}
	c.eventQueue = append(c.eventQueue, e)
	c.eventBytes += len(e.Params)
	select {
	case c.eventWake <- struct{}{}:
	default:
	}
	return nil
}

func (c *Conn) dispatchEvents() {
	for {
		select {
		case <-c.done:
			return
		case <-c.eventWake:
		}
		for {
			c.mu.Lock()
			if len(c.eventQueue) == 0 {
				c.mu.Unlock()
				break
			}
			e := c.eventQueue[0]
			c.eventBytes -= len(e.Params)
			c.eventQueue[0] = Event{}
			c.eventQueue = c.eventQueue[1:]
			c.mu.Unlock()
			select {
			case <-c.done:
				return
			case c.events <- e:
			}
		}
	}
}

// ACKs use one worker, independent of event consumption; the pipe reader stays
// available for command replies even when frame consumers are slow or absent.
func (c *Conn) ackFrames() {
	for {
		select {
		case <-c.done:
			return
		case e := <-c.frameAcks:
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			_ = c.Call(ctx, e.Session, "Page.screencastFrameAck", e.Params, nil)
			cancel()
		}
	}
}
