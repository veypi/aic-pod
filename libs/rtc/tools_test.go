package rtc_test

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/pion/webrtc/v4"
	"github.com/veypi/aic-pod/libs/hostauth"
	tool "github.com/veypi/aic-pod/libs/hosts_tool"
	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/aic-pod/libs/rtc"
	hosts "github.com/veypi/aic-pod/protocol/hosts_rtc"
	rtcwire "github.com/veypi/aic-pod/protocol/hosts_rtc"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
	"regexp"
	"sync"
	"testing"
	"time"
)

type rtcTools struct{ d *tool.Dispatcher }

func (b rtcTools) HandleTool(ctx context.Context, c tool.Caller, r wire.Request) wire.Response {
	return b.d.Handle(ctx, c, r)
}
func (b rtcTools) OpenToolStream(ctx context.Context, c tool.Caller, in wire.Invocation) (tool.Stream, error) {
	return b.d.OpenStream(ctx, c, in)
}
func (b rtcTools) DisconnectTools(c tool.Caller) { b.d.Disconnect(c) }
func TestRTCToolsWithoutBusinessSession(t *testing.T) {
 key,_:=hosts.DirectKey("secret","host_1")
 auth,err:=hostauth.NewAccess(hostauth.AccessConfig{HostID:"host_1",UserID:"owner",CredentialVersion:1,Key:key});if err!=nil{t.Fatal(err)}
 defer auth.RevokeAll()
	d := tool.New(tool.Config{})
	defer d.Close(context.Background())
	calls := 0
	source := &duplexFixture{out: make(chan []byte, 16), gate: make(chan struct{}), closed: make(chan struct{})}
	if err = d.RegisterCommand(tool.DefineCommand("counter", tool.Bind(tool.Spec{Name: "next", Access: 1}, func(ctx context.Context, c tool.Caller, _ struct{}) (int, error) { calls++; return calls, nil }), tool.BindStream(tool.Spec{Name: "duplex", Access: 1}, func(context.Context, tool.Caller, struct{}) (tool.Stream, error) { return source, nil }))); err != nil {
		t.Fatal(err)
	}
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	dc, err := pc.CreateDataChannel(rtcwire.Channel, nil)
	if err != nil {
		t.Fatal(err)
	}
	opened := make(chan struct{})
	dc.OnOpen(func() { close(opened) })
	responses := make(chan wire.Response, 8)
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		var r wire.Response
		if json.Unmarshal(m.Data, &r) == nil {
			responses <- r
		}
	})
	var signalMu sync.Mutex
	var candidates []webrtc.ICECandidateInit
	remote := false
	service, err := rtc.New(rtc.Config{HostID: "host_1", Authorization: auth, Tools: rtcTools{d}, Send: func(sig *proto.RtcSignal) {
		signalMu.Lock()
		defer signalMu.Unlock()
		if sig.Kind == proto.RtcAnswer {
			if e := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sig.SDP}); e != nil {
				t.Error(e)
			}
			remote = true
			for _, c := range candidates {
				_ = pc.AddICECandidate(c)
			}
			candidates = nil
		}
		if sig.Kind == proto.RtcCandidate {
			var c webrtc.ICECandidateInit
			_ = json.Unmarshal([]byte(sig.Candidate), &c)
			if remote {
				_ = pc.AddICECandidate(c)
			} else {
				candidates = append(candidates, c)
			}
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err = pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gathered:
	case <-time.After(5 * time.Second):
		t.Fatal("ICE timeout")
	}
	fp := regexp.MustCompile(`(?m)^a=fingerprint:(sha-256 [^\r\n]+)`).FindStringSubmatch(pc.LocalDescription().SDP)[1]
	service.HandleSignal(&proto.RtcSignal{PC: "pc_tools", Kind: proto.RtcOffer, SDP: pc.LocalDescription().SDP})
	select {
	case <-opened:
	case <-time.After(10 * time.Second):
		t.Fatal("channel timeout")
	}
	request := func(r wire.Request, channels ...string) wire.Response {
		t.Helper()
		r.Protocol = rtcwire.Protocol
		r.ID = wire.NewID("r_")
		envelope := rtcwire.Request{Request: r}
		if len(channels) > 0 {
			envelope.Channel = channels[0]
		}
		raw, _ := json.Marshal(envelope)
		if err := dc.SendText(string(raw)); err != nil {
			t.Fatal(err)
		}
		select {
		case v := <-responses:
			if v.ID != r.ID || v.Protocol != rtcwire.Protocol {
				t.Fatal("response identity")
			}
			return v
		case <-time.After(5 * time.Second):
			t.Fatal("request timeout")
			return wire.Response{}
		}
	}
	if r := request(wire.Request{Action: "catalog"}); r.Error == nil {
		t.Fatal("unauthenticated tools admitted")
	}
	key, _ = hosts.DirectKey("secret", "host_1")
	ticket, err := hosts.SignTicket(key, hosts.Ticket{HostID: "host_1", UserID: "owner", CredentialVersion: 1, PCID: "pc_tools", Fingerprint: fp}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if r := request(wire.Request{Action: "hello", Ticket: ticket}); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := request(wire.Request{Action: "call", Argv: []string{"counter", "next"}}); r.Error != nil || r.Result != float64(1) {
		t.Fatalf("%+v", r)
	}
	if r := request(wire.Request{Action: "session.open"}); r.Error == nil {
		t.Fatal("legacy business session accepted")
	}
	if r := request(wire.Request{Action: "call", Argv: []string{"counter", "next"}}); r.Error != nil || r.Result != float64(2) {
		t.Fatalf("%+v", r)
	}
	// Channel setup is the only request. Neither duplex payloads nor local
	// consumers exchange per-message RPCs, even while tool input is blocked.
	label := rtcwire.StreamPrefix + "duplex"
	channel, err := pc.CreateDataChannel(label, nil)
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	channel.OnOpen(func() { close(ready) })
	packets := make(chan []byte, 16)
	channel.OnMessage(func(m webrtc.DataChannelMessage) {
		if m.IsString {
			t.Error("text wrapper on opaque channel")
		}
		packets <- m.Data
	})
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("stream channel did not open")
	}
	if r := request(wire.Request{Action: "stream.open", Call: &wire.Invocation{Domain: "exec", Command: "counter", Method: "duplex", Args: json.RawMessage(`{}`)}}, label); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := request(wire.Request{Action: "stream.send"}); r.Error == nil {
		t.Fatal("RPC stream send retained")
	}
	input := []byte{0, 255, 123, 0, 1}
	for range 3 {
		if err := channel.Send(input); err != nil {
			t.Fatal(err)
		}
	}
	output := []byte{9, 0, 255}
	source.out <- output
	select {
	case packet := <-packets:
		if !bytes.Equal(packet, output) {
			t.Fatalf("payload changed: %v", packet)
		}
	case <-time.After(time.Second):
		t.Fatal("output blocked behind input")
	}
	close(source.gate)
	for range 3 {
		select {
		case packet := <-packets:
			if !bytes.Equal(packet, input) {
				t.Fatal(packet)
			}
		case <-time.After(time.Second):
			t.Fatal("input waited for a per-message request")
		}
	}
	_ = channel.Close()
	select {
	case <-source.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("tool endpoint leaked after channel close")
	}

}

type duplexFixture struct {
	out    chan []byte
	gate   chan struct{}
	closed chan struct{}
	once   sync.Once
}

func (s *duplexFixture) Send(ctx context.Context, b []byte) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.gate:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case s.out <- b:
		return nil
	}
}
func (s *duplexFixture) Recv(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case b := <-s.out:
		return b, nil
	}
}
func (s *duplexFixture) Close() error { s.once.Do(func() { close(s.closed) }); return nil }

func decode[T any](t *testing.T,v any)T{t.Helper();raw,_:=json.Marshal(v);var out T;if err:=json.Unmarshal(raw,&out);err!=nil{t.Fatal(err)};return out}
