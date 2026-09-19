package rtc

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/veypi/aic-pod/libs/hostcmd"
	"github.com/veypi/aic-pod/protocol/hosts"
)

type liveBackend interface {
	OpenLive(context.Context, string, hostcmd.RequestCall) (hostcmd.Live, error)
}
type liveStream struct {
	id, session, connection string
	source                  hostcmd.Live
	sourceMu                sync.Mutex
	ctx                     context.Context
	cancel                  context.CancelFunc
	inputs                  chan json.RawMessage
	ack                     chan string
	once                    sync.Once
}

func (l *liveStream) stop() {
	l.once.Do(func() {
		l.cancel()
		l.sourceMu.Lock()
		defer l.sourceMu.Unlock()
		if l.source != nil {
			l.source.Close()
		}
	})
}
func (p *peer) endLive(l *liveStream, err error) {
	p.mu.Lock()
	exists := p.lives[l.id] == l
	if exists {
		delete(p.lives, l.id)
	}
	p.mu.Unlock()
	l.stop()
	if exists {
		var fault *hosts.Fault
		if err != nil {
			fault = hosts.AsFault(err)
		}
		p.emit("live.closed", map[string]any{"stream_id": l.id, "error": fault})
	}
}
func (p *peer) openLive(connection string, raw json.RawMessage) (any, func(), error) {
	var args struct {
		hostcmd.RequestCall
		Stream string `json:"stream_id"`
	}
	if err := hosts.Decode(raw, &args); err != nil {
		return nil, nil, err
	}
	if !hosts.ValidID(args.Stream) {
		return nil, nil, hosts.Fail("invalid_argument", "Invalid live stream identity")
	}
	backend, ok := p.s.cfg.Commands.(liveBackend)
	if !ok {
		return nil, nil, hosts.Fail("unsupported", "Live methods are unavailable")
	}
	if _, err := p.s.cfg.Commands.Authorization().DataCaller(connection); err != nil {
		return nil, nil, err
	}
	if err := p.s.cfg.Commands.CheckSession(connection, args.SessionID); err != nil {
		return nil, nil, err
	}
	p.mu.Lock()
	if p.closed || p.live == nil || len(p.lives) >= 4 || len(p.liveIDs) >= 4096 || p.liveIDs[args.Stream] {
		p.mu.Unlock()
		return nil, nil, hosts.Fail("overloaded", "Live channel unavailable or stream limit reached")
	}
	if p.lives == nil {
		p.lives = map[string]*liveStream{}
		p.liveIDs = map[string]bool{}
	}
	ctx, cancel := context.WithCancel(p.ctx)
	l := &liveStream{id: args.Stream, session: args.SessionID, connection: connection, ctx: ctx, cancel: cancel, inputs: make(chan json.RawMessage, 128), ack: make(chan string, 1)}
	// Reserve before provider open so concurrent requests share the limit.
	p.liveIDs[l.id] = true
	p.lives[l.id] = l
	p.mu.Unlock()
	source, err := backend.OpenLive(ctx, connection, args.RequestCall)
	if err != nil {
		cancel()
		p.mu.Lock()
		delete(p.lives, l.id)
		p.mu.Unlock()
		return nil, nil, err
	}
	// Opening may race peer/session close; publish under the source lock.
	l.sourceMu.Lock()
	l.source = source
	closed := ctx.Err() != nil
	if closed {
		source.Close()
	}
	l.sourceMu.Unlock()
	if closed {
		p.endLive(l, hosts.Fail("cancelled", "Live open cancelled"))
		return nil, nil, hosts.Fail("cancelled", "Live open cancelled")
	}
	return map[string]any{"stream_id": l.id, "max_item_bytes": hostcmd.MaxLiveBytes}, func() {
		go p.liveInputs(l)
		go p.liveFrames(l)
	}, nil
}
func (p *peer) liveInputs(l *liveStream) {
	for {
		select {
		case <-l.ctx.Done():
			return
		case data := <-l.inputs:
			if err := l.source.Send(data); err != nil {
				p.endLive(l, err)
				return
			}
		}
	}
}
func (p *peer) liveFrames(l *liveStream) {
	for seq := int64(0); ; seq++ {
		item, err := l.source.Recv()
		if err != nil {
			p.endLive(l, err)
			return
		}
		if len(item.Data) > hostcmd.MaxLiveBytes || len(item.Metadata) > 768 || !json.Valid(item.Metadata) {
			p.endLive(l, hosts.Fail("overloaded", "Live item exceeds limit"))
			return
		}
		itemID, _ := hosts.NewID("li_")
		for offset := 0; ; {
			if err := p.s.cfg.Commands.CheckSession(l.connection, l.session); err != nil {
				p.endLive(l, err)
				return
			}
			end := min(offset+hosts.ChunkBytes, len(item.Data))
			header := hosts.Frame{StreamID: l.id, ItemID: itemID, Seq: seq, Offset: int64(offset), Final: end == len(item.Data)}
			if offset == 0 {
				header.Size = int64(len(item.Data))
				header.Metadata = item.Metadata
			}
			raw, e := hosts.EncodeFrame(header, item.Data[offset:end])
			if e == nil {
				e = p.sendRaw(l.ctx, p.live, raw, false)
			}
			if e != nil {
				p.endLive(l, e)
				return
			}
			offset = end
			if header.Final {
				break
			}
		}
		// One complete frame in transit. Latest-state consumers acknowledge receipt
		// and decode in parallel; lossless consumers acknowledge consumption.
		// The provider retains only its latest frame while waiting for this credit.
		timer := time.NewTimer(30 * time.Second)
		select {
		case <-l.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			p.endLive(l, hosts.Fail("expired", "Live viewer stopped consuming"))
			return
		case ack := <-l.ack:
			timer.Stop()
			if ack != itemID {
				p.endLive(l, hosts.Fail("invalid_argument", "Invalid live acknowledgement"))
				return
			}
		}
	}
}
func (p *peer) liveEvent(e hosts.Event) {
	var args struct {
		Stream string          `json:"stream_id"`
		Item   string          `json:"item_id,omitempty"`
		Data   json.RawMessage `json:"data,omitempty"`
	}
	if hosts.Decode(e.Data, &args) != nil {
		p.s.drop(p)
		return
	}
	p.mu.Lock()
	l := p.lives[args.Stream]
	p.mu.Unlock()
	if l == nil {
		return
	}
	if e.Event == "live.ack" {
		select {
		case l.ack <- args.Item:
		default:
			p.endLive(l, hosts.Fail("overloaded", "Too many live acknowledgements"))
		}
	} else {
		if _, err := hosts.CanonicalArgs(args.Data); err != nil {
			p.endLive(l, err)
			return
		}
		select {
		case l.inputs <- args.Data:
		default:
			p.endLive(l, hosts.Fail("overloaded", "Live input queue is full"))
		}
	}
}
