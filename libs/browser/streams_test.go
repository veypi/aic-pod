package browser

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	tool "github.com/veypi/aic-pod/libs/hosts_tool"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
)

func TestInputBatchCoalescing(t *testing.T) {
	wheel := func(dy float64) inputEvent { return inputEvent{Type: "wheel", X: 10, Y: 20, DeltaY: dy} }
	target, modifier := wheel(3), wheel(4)
	target.X, modifier.Modifiers = 11, 8
	events := []inputEvent{wheel(1), wheel(2), wheel(-1), wheel(-2), target, modifier,
		{Type: "pointer.down", Button: "left"}, wheel(5), {Type: "pointer.up", Button: "left"},
		{Type: "pointer.move", X: 1}, {Type: "pointer.move", X: 2}, {Type: "text", Text: "x"},
		{Type: "key.down", Key: "a"}, {Type: "key.up", Key: "a"}}
	want := append([]inputEvent(nil), events[4:9]...)
	want = append(want, events[10:]...)
	if got := coalesceInput(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestInputBatchRejectsInvalidTailBeforeDispatch(t *testing.T) {
	i := inputFixture(t)
	ctx, p, l := i.ctx, i.p, i.l
	// A nil CDP connection would panic if the valid first event were dispatched.
	raw, _ := json.Marshal(inputBatch{Seq: 1, Document: "doc", Events: []inputEvent{
		{Type: "key.down", Key: "a"}, {Type: "wheel", X: 800, DeltaY: 1},
	}})
	if err := i.Send(ctx, raw); err == nil {
		t.Fatal("invalid tail accepted")
	}
	if i.seq != 0 || len(l.keys) != 0 || p.info.Revision != 0 || p.lease != nil {
		t.Fatal("invalid batch mutated input state")
	}
	raw, _ = json.Marshal(inputBatch{Seq: 1, Document: "doc", Events: []inputEvent{{Type: "reset"}}})
	if err := i.Send(ctx, raw); err != nil {
		t.Fatal(err)
	}
	if err := i.Send(ctx, raw); err == nil {
		t.Fatal("duplicate batch accepted")
	}
}

func TestInputBatchValidationLimits(t *testing.T) {
	for _, events := range [][]inputEvent{nil, make([]inputEvent, 33), {{Type: "text", Text: string(bytes.Repeat([]byte{'x'}, 16385))}}, {{Type: "wheel", DeltaY: 100001}}, {{Type: "key.down"}}, {{Type: "pointer.down"}}, {{Type: "unknown"}}} {
		if err := validateInput(inputBatch{Seq: 1, Document: "doc", Events: events}, 800, 600, nil); err == nil {
			t.Fatalf("accepted %+v", events)
		}
	}
}

func TestFramesNeverRewindOrMixPartialImages(t *testing.T) {
	f := &frameStream{ctx: context.Background(), frames: make(chan frameData, 1)}
	info := PageInfo{ID: "p", Document: "doc", Width: 800, Height: 600}
	f.offer(makeFrame(bytes.Repeat([]byte{2}, frameChunkBytes+5), info, 2))
	packet, err := f.Recv(context.Background())
	first := readFramePacket(t, packet)
	if err != nil || first.Final || len(first.Data) != frameChunkBytes {
		t.Fatalf("first: %+v %v", first, err)
	}
	f.offer(makeFrame([]byte{3}, info, 3))
	f.offer(makeFrame([]byte{1}, info, 1)) // A slow initial capture arrives late.
	f.offer(makeFrame([]byte{4}, info, 4)) // Pending intermediate frame is replaced.
	packet, err = f.Recv(context.Background())
	last := readFramePacket(t, packet)
	if err != nil || !last.Final || !bytes.Equal(last.Data, bytes.Repeat([]byte{2}, 5)) {
		t.Fatalf("mixed frame: %+v %v", last, err)
	}
	packet, err = f.Recv(context.Background())
	next := readFramePacket(t, packet)
	if err != nil || !next.Final || !bytes.Equal(next.Data, []byte{4}) {
		t.Fatalf("rewound frame: %+v %v", next, err)
	}
	var meta struct {
		Seq uint64 `json:"frame_seq"`
	}
	if json.Unmarshal(next.Metadata, &meta) != nil || meta.Seq != 4 {
		t.Fatal(string(next.Metadata))
	}
}

func TestCompositorFramesKeepCaptureOrder(t *testing.T) {
	f := &frameStream{ctx: context.Background(), frames: make(chan frameData, 1)}
	p := &page{casting: true, viewers: map[*frameStream]bool{f: true}}
	p.deliverFrame([]byte{2}, 20)
	p.deliverFrame([]byte{1}, 10)
	p.deliverFrame([]byte{1}, 20)
	packet, err := f.Recv(context.Background())
	item := readFramePacket(t, packet)
	if err != nil || !bytes.Equal(item.Data, []byte{2}) || p.frameSeq != 1 {
		t.Fatalf("late image accepted: %+v %v", item, err)
	}
	p.casting = false
	p.deliverFrame([]byte{3}, 30)
	if len(f.frames) != 0 {
		t.Fatal("stopped source delivered a frame")
	}
}

func readFramePacket(t *testing.T, packet []byte) struct {
	Metadata json.RawMessage
	Final    bool
	Data     []byte
} {
	t.Helper()
	var chunk struct {
		Metadata json.RawMessage
		Final    bool
		Data     []byte
	}
	if len(packet) < 4 {
		t.Fatal("short packet")
	}
	n := int(binary.BigEndian.Uint32(packet))
	if n > len(packet)-4 {
		t.Fatal("short header")
	}
	if err := json.Unmarshal(packet[4:4+n], &chunk); err != nil {
		t.Fatal(err)
	}
	chunk.Data = packet[4+n:]
	return chunk
}

func TestInputAcceptsAndCoalescesBeforeExecutionAndReportsAsyncFailure(t *testing.T) {
	i := inputFixture(t)
	ctx, p := i.ctx, i.p
	// No worker or CDP connection exists yet: receipt cannot await execution.
	for seq := uint64(1); seq <= 2000; seq++ {
		raw, _ := json.Marshal(inputBatch{Seq: seq, Document: "doc", Events: []inputEvent{{Type: "wheel", X: 1, Y: 1, DeltaY: 1}}})
		if err := i.Send(ctx, raw); err != nil {
			t.Fatal(err)
		}
	}
	if len(i.queue) != 1 || i.queue[0].DeltaY != 1 {
		t.Fatalf("input not coalesced: %+v", i.queue)
	}
	p.info.Document = "new"
	go i.run()
	readCtx, readCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer readCancel()
	_, err := i.Recv(readCtx)
	if err == nil || wire.AsFault(err).Code != "stale_input" {
		t.Fatalf("asynchronous failure missing: %v", err)
	}
	select {
	case <-i.done:
	case <-time.After(time.Second):
		t.Fatal("failed input retained lease")
	}
}

// Unit fixture without a worker: acceptance can be exercised while execution
// is stalled, without connecting Chrome or waiting for native input dispatch.
func inputFixture(t *testing.T) *inputStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	p := &page{gate: make(chan struct{}, 1), info: PageInfo{Document: "doc", Width: 800, Height: 600}}
	i := &inputStream{p: p, ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1), done: make(chan struct{}), plannedKeys: map[string]bool{}, caller: tool.Caller{Subject: "owner", ConnectionID: "rtc", Level: 3, ExpiresAt: time.Now().Add(time.Minute)}}
	i.l = &lease{connection: i.caller.ConnectionID, input: i, keys: map[string]bool{}}
	t.Cleanup(func() { i.Close() })
	return i
}

func TestAutomaticInputControlAndIdleReuse(t *testing.T) {
	i := inputFixture(t)
	send := func(seq uint64, kind string) {
		t.Helper()
		raw, _ := json.Marshal(inputBatch{Seq: seq, Document: "doc", Events: []inputEvent{{Type: kind, X: 1, Y: 1}}})
		if err := i.Send(i.ctx, raw); err != nil {
			t.Fatal(err)
		}
	}
	if i.p.lease != nil {
		t.Fatal("channel reserved page before input")
	}
	send(1, "reset")
	if i.p.lease != nil {
		t.Fatal("reset entered control")
	}
	send(2, "pointer.move")
	if i.p.lease != i.l {
		t.Fatal("first input did not enter control")
	}
	other := i.caller
	other.ConnectionID = "nats:owner"
	if err := i.p.write(i.ctx, other, func() error { t.Fatal("AI entered active page"); return nil }); wire.AsFault(err).Code != "control_busy" {
		t.Fatal(err)
	}
	// Simulate the worker having consumed accepted input, then real inactivity.
	i.queue = nil
	i.l.expires = time.Now().Add(-time.Second)
	i.releaseControl(false, true)
	if i.p.lease != nil || i.ctx.Err() != nil {
		t.Fatal("idle expiry closed channel or retained control")
	}
	if err := i.p.write(i.ctx, other, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	send(3, "pointer.move")
	if i.p.lease != i.l {
		t.Fatal("same channel did not regain control")
	}
	i.Close()
	if i.p.lease != nil {
		t.Fatal("closed input retained control")
	}
}

func TestInputLatestAcrossMixedMotionAndDirection(t *testing.T) {
	var events []inputEvent
	for n := range 2000 {
		events = append(events, inputEvent{Type: "pointer.move", X: float64(n)}, inputEvent{Type: "wheel", X: float64(n), DeltaY: float64(n%2*2 - 1)})
	}
	got := coalesceInput(events)
	if len(got) != 2 || got[0].X != 1999 || got[1].DeltaY != 1 {
		t.Fatalf("stale motion retained: %+v", got)
	}
	events = append(events, inputEvent{Type: "pointer.down", Button: "left"}, inputEvent{Type: "wheel", DeltaY: 5}, inputEvent{Type: "wheel", DeltaY: -2}, inputEvent{Type: "pointer.up", Button: "left"})
	got = coalesceInput(events)
	if len(got) != 5 || got[2].Type != "pointer.down" || got[3].DeltaY != -2 || got[4].Type != "pointer.up" {
		t.Fatalf("discrete barrier lost: %+v", got)
	}
}
