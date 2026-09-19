package rtc_test

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/veypi/aic-pod/libs/host"
	"github.com/veypi/aic-pod/libs/hostcmd"
	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/aic-pod/libs/rtc"
	"github.com/veypi/aic-pod/protocol/hosts"
)

type client struct {
	control, data *webrtc.DataChannel
	mu            sync.Mutex
	pending       map[string]chan hosts.Response
	events        chan hosts.Event
	frames        chan []byte
}

func decode[T any](t *testing.T, v any) T {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out T
	if err = json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func (c *client) request(t *testing.T, dc *webrtc.DataChannel, method string, args any) hosts.Response {
	t.Helper()
	id, _ := hosts.NewID("req_")
	body, _ := json.Marshal(args)
	raw, _ := json.Marshal(hosts.Request{V: 1, Type: "request", ID: id, Method: method, Params: body})
	ready := make(chan hosts.Response, 1)
	c.mu.Lock()
	c.pending[id] = ready
	c.mu.Unlock()
	if err := dc.SendText(string(raw)); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-ready:
		return r
	case <-time.After(5 * time.Second):
		t.Fatalf("request timed out: %s", method)
		return hosts.Response{}
	}
}
func (c *client) call(t *testing.T, method string, args any) any {
	t.Helper()
	r := c.request(t, c.control, method, args)
	if !r.OK {
		t.Fatalf("%s: %+v", method, r.Error)
	}
	return r.Result
}
func (c *client) event(t *testing.T, name string, value any) {
	t.Helper()
	data, _ := json.Marshal(value)
	raw, _ := json.Marshal(hosts.Event{V: 1, Type: "event", Event: name, Data: data})
	if err := c.control.SendText(string(raw)); err != nil {
		t.Fatal(err)
	}
}
func (c *client) next(t *testing.T, name, stream string) hosts.Event {
	t.Helper()
	for {
		select {
		case event := <-c.events:
			var data struct {
				ID string `json:"stream_id"`
			}
			_ = json.Unmarshal(event.Data, &data)
			if event.Event == name && data.ID == stream {
				return event
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("event timed out: %s", name)
			return hosts.Event{}
		}
	}
}
func setup(t *testing.T) (*client, *host.CommandService, string) {
	t.Helper()
	backend, err := host.New(host.Options{Key: "host_1.1.secret.owner", WorkDir: t.TempDir()}).NewCommandService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close(context.Background()) })
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	c := &client{pending: map[string]chan hosts.Response{}, events: make(chan hosts.Event, 256), frames: make(chan []byte, 256)}
	opened := make(chan struct{}, 2)
	onMessage := func(m webrtc.DataChannelMessage) {
		if !m.IsString {
			h, data, err := hosts.DecodeFrame(m.Data)
			if err != nil {
				t.Error(err)
				return
			}
			c.mu.Lock()
			ch := c.pending[h.RequestID]
			delete(c.pending, h.RequestID)
			c.mu.Unlock()
			if ch != nil {
				ch <- hosts.Reply(h.RequestID, map[string]any{"header": h, "data": data}, nil)
			}
			return
		}
		var typ struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(m.Data, &typ)
		if typ.Type == "event" {
			var event hosts.Event
			_ = json.Unmarshal(m.Data, &event)
			c.events <- event
			return
		}
		var r hosts.Response
		_ = json.Unmarshal(m.Data, &r)
		c.mu.Lock()
		ch := c.pending[r.ID]
		delete(c.pending, r.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- r
		}
	}
	c.control, err = pc.CreateDataChannel("hosts-control", nil)
	if err != nil {
		t.Fatal(err)
	}
	c.data, err = pc.CreateDataChannel("hosts-data", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, dc := range []*webrtc.DataChannel{c.control, c.data} {
		dc.OnOpen(func() { opened <- struct{}{} })
		dc.OnMessage(onMessage)
	}
	var signalMu sync.Mutex
	var candidates []webrtc.ICECandidateInit
	remote := false
	service, err := rtc.New(rtc.Config{HostID: "host_1", Commands: backend, Send: func(sig *proto.RtcSignal) {
		signalMu.Lock()
		defer signalMu.Unlock()
		if sig.Kind == proto.RtcAnswer {
			if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sig.SDP}); err != nil {
				t.Error(err)
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
	t.Cleanup(service.Close)
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
		t.Fatal("ICE gathering timed out")
	}
	fingerprint := regexp.MustCompile(`(?m)^a=fingerprint:(sha-256 [^\r\n]+)`).FindStringSubmatch(pc.LocalDescription().SDP)[1]
	service.HandleSignal(&proto.RtcSignal{PC: "pc_1", Kind: proto.RtcOffer, SDP: pc.LocalDescription().SDP})
	for range 2 {
		select {
		case <-opened:
		case <-time.After(10 * time.Second):
			t.Fatal("RTC open timed out")
		}
	}
	key, _ := hosts.DirectKey("secret", "host_1")
	ticket, err := hosts.SignTicket(key, hosts.Ticket{HostID: "host_1", UserID: "owner", CredentialVersion: 1, PCID: "pc_1", Fingerprint: fingerprint}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if r := c.request(t, c.control, "session.open", map[string]any{}); r.OK {
		t.Fatal("unauthenticated session accepted")
	}
	hello := decode[struct {
		Connection string `json:"connection_id"`
		Epoch      string `json:"runtime_epoch"`
		Token      string `json:"data_token"`
	}](t, c.call(t, "hello", map[string]any{"protocol": hosts.Protocol, "ticket": ticket}))
	if r := c.request(t, c.data, "data.bind", map[string]any{"connection_id": hello.Connection, "runtime_epoch": hello.Epoch, "token": hello.Token}); !r.OK {
		t.Fatal(r.Error)
	}
	session := decode[hostcmd.Session](t, c.call(t, "session.open", map[string]any{}))
	return c, backend, session.ID
}
func TestRTCGenericCommandsAndRawByteStreams(t *testing.T) {
	c, backend, session := setup(t)
	other := hostcmd.Caller{Subject: "owner", ConnectionID: "other_authenticated_connection", ExpiresAt: time.Now().Add(time.Minute)}
	foreign, err := backend.Runtime.Open(other)
	if err != nil {
		t.Fatal(err)
	}
	if response := c.request(t, c.control, "session.close", map[string]any{"session_id": foreign.ID}); response.OK {
		t.Fatal("closed a session bound to another connection")
	}
	if err := backend.Runtime.CheckSession(other, foreign.ID); err != nil {
		t.Fatal("foreign session was mutated", err)
	}
	// Generic dispatch: the transport knows nothing about fs roots.
	raw, _ := json.Marshal(map[string]any{})
	in := hosts.Invocation{SessionID: session, RuntimeEpoch: backend.Epoch(), OperationID: "op_roots", Command: "fs", Method: "roots", Args: raw}
	c.call(t, "invoke", in)
	op := decode[hosts.Operation](t, c.call(t, "operation.get", map[string]any{"session_id": session, "operation_id": "op_roots", "wait_ms": 1000}))
	if op.Status != "succeeded" {
		t.Fatalf("operation: %+v", op)
	}
	for _, payload := range [][]byte{nil, append([]byte{0, 255, 0, 128}, bytes.Repeat([]byte("原始\r\n"), 16000)...)} {
		stream, _ := hosts.NewID("st_")
		item := decode[hosts.StreamItem](t, c.call(t, "bytes.create", map[string]any{"session_id": session, "stream_id": stream, "size": len(payload)}))
		offset, seq := 0, int64(0)
		for {
			n := min(hosts.ChunkBytes, len(payload)-offset)
			h := hosts.Frame{SessionID: session, StreamID: stream, ItemID: item.ItemID, Seq: seq, Offset: int64(offset), Final: offset+n == len(payload)}
			ack := decode[hostcmd.TransferStatus](t, c.chunk(t, h, payload[offset:offset+n]))
			offset += n
			seq++
			if ack.Offset != int64(offset) || ack.Seq != seq {
				t.Fatal("invalid upload acknowledgement")
			}
			if h.Final {
				break
			}
		}
		source := decode[hostcmd.ByteSource](t, c.call(t, "bytes.seal", map[string]any{"session_id": session, "stream_id": stream, "actual_size": len(payload)}))
		c.call(t, "stream.cancel", map[string]any{"session_id": session, "stream_id": stream})
		stream, _ = hosts.NewID("st_")
		read := decode[hosts.StreamItem](t, c.call(t, "bytes.read", map[string]any{"session_id": session, "stream_id": stream, "ref": source.Ref}))
		var got bytes.Buffer
		for seq = 0; ; seq++ {
			n := min(hosts.ChunkBytes, len(payload)-got.Len())
			f := decode[struct {
				Header hosts.Frame `json:"header"`
				Data   []byte      `json:"data"`
			}](t, c.call(t, "stream.pull", map[string]any{"session_id": session, "stream_id": stream, "seq": seq, "offset": got.Len(), "bytes": n}))
			if f.Header.SessionID != session || f.Header.StreamID != stream || f.Header.ItemID != read.ItemID || f.Header.Seq != seq || f.Header.Offset != int64(got.Len()) || len(f.Data) != n {
				t.Fatal("invalid file frame")
			}
			got.Write(f.Data)
			if f.Header.Final {
				break
			}
		}
		if !bytes.Equal(got.Bytes(), payload) {
			t.Fatal("raw bytes changed over RTC")
		}
		c.call(t, "stream.cancel", map[string]any{"session_id": session, "stream_id": stream})
		c.call(t, "resource.release", map[string]any{"session_id": session, "ref": source.Ref})
	}
	c.call(t, "session.close", map[string]any{"session_id": session})
	c.call(t, "session.close", map[string]any{"session_id": session})
	if _, err := backend.Upload(context.Background(), "not_authenticated", session, bytes.NewReader(nil), nil, "", ""); err == nil {
		t.Fatal("byte authorization bypass")
	}
}

func (c *client) chunk(t *testing.T, h hosts.Frame, data []byte) any {
	t.Helper()
	h.RequestID, _ = hosts.NewID("req_")
	raw, err := hosts.EncodeFrame(h, data)
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan hosts.Response, 1)
	c.mu.Lock()
	c.pending[h.RequestID] = ready
	c.mu.Unlock()
	if err = c.data.Send(raw); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-ready:
		if !r.OK {
			t.Fatal(r.Error)
		}
		return r.Result
	case <-time.After(5 * time.Second):
		t.Fatal("chunk timeout")
		return nil
	}
}

func TestFileSessionHandoffPreservesChunksAndScopes(t *testing.T) {
	c, backend, _ := setup(t)
	session := decode[hostcmd.Session](t, c.call(t, "session.open", map[string]any{"scope": []string{"fs"}}))
	stream := "handoff_stream"
	item := decode[hosts.StreamItem](t, c.call(t, "bytes.create", map[string]any{"session_id": session.ID, "stream_id": stream, "size": 6}))
	first := hosts.Frame{SessionID: session.ID, StreamID: stream, ItemID: item.ItemID, Seq: 0, Offset: 0}
	c.chunk(t, first, []byte("abc"))
	key, _ := hosts.ProxyKey("secret", "host_1")
	conn := ""
	proxy := func(packet hosts.Packet) hosts.Packet {
		t.Helper()
		token, err := hosts.SignProxy(key, hosts.ProxyEnvelope{HostID: "host_1", UserID: "owner", CredentialVersion: 1, ConnectionID: conn, Scope: []string{"fs"}, Packet: packet}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return backend.HandleProxy(context.Background(), token)
	}
	request := func(method string, args any) hosts.Response {
		t.Helper()
		params, _ := json.Marshal(args)
		packet := proxy(hosts.JSONPacket(hosts.Request{V: 1, Type: "request", ID: "proxy_req", Method: method, Params: params}))
		var result hosts.Response
		if json.Unmarshal(packet.Data, &result) != nil {
			t.Fatal("invalid response")
		}
		return result
	}
	call := func(method string, args any) any {
		t.Helper()
		r := request(method, args)
		if !r.OK {
			t.Fatal(method, r.Error)
		}
		return r.Result
	}
	hello := decode[map[string]any](t, call("hello", map[string]any{"protocol": hosts.Protocol}))
	conn = hello["connection_id"].(string)
	resumed := decode[hostcmd.Session](t, call("session.resume", map[string]any{"session_id": session.ID, "runtime_epoch": session.RuntimeEpoch, "resume_token": session.ResumeToken, "replace_connection": true}))
	if resumed.ID != session.ID || resumed.ResumeToken == session.ResumeToken {
		t.Fatal("session identity or token rotation")
	}
	if r := c.request(t, c.control, "stream.status", map[string]any{"session_id": session.ID, "stream_id": stream}); r.OK {
		t.Fatal("old connection still controls session")
	}
	// Losing the first acknowledgement does not append the same bytes twice.
	first.RequestID = "retry_first"
	raw, _ := hosts.EncodeFrame(first, []byte("abc"))
	packet := proxy(hosts.Packet{Binary: true, Data: raw})
	var ack hosts.Response
	_ = json.Unmarshal(packet.Data, &ack)
	if !ack.OK || decode[hostcmd.TransferStatus](t, ack.Result).Offset != 3 {
		t.Fatal("chunk recovery", ack)
	}
	status := decode[hostcmd.TransferStatus](t, call("stream.status", map[string]any{"session_id": session.ID, "stream_id": stream}))
	if status.Seq != 1 || status.Offset != 3 {
		t.Fatal(status)
	}
	final := hosts.Frame{RequestID: "last_chunk", SessionID: session.ID, StreamID: stream, ItemID: item.ItemID, Seq: 1, Offset: 3, Final: true}
	raw, _ = hosts.EncodeFrame(final, []byte("def"))
	packet = proxy(hosts.Packet{Binary: true, Data: raw})
	_ = json.Unmarshal(packet.Data, &ack)
	if !ack.OK {
		t.Fatal(ack.Error)
	}
	source := decode[hostcmd.ByteSource](t, call("bytes.seal", map[string]any{"session_id": session.ID, "stream_id": stream, "actual_size": 6}))
	again := decode[hostcmd.ByteSource](t, call("bytes.seal", map[string]any{"session_id": session.ID, "stream_id": stream, "actual_size": 6}))
	if source.Ref != again.Ref {
		t.Fatal("seal replay made a new source")
	}
	call("bytes.read", map[string]any{"session_id": session.ID, "stream_id": "download_handoff", "ref": source.Ref})
	p, _ := json.Marshal(map[string]any{"session_id": session.ID, "stream_id": "download_handoff", "seq": 0, "offset": 0, "bytes": 6})
	packet = proxy(hosts.JSONPacket(hosts.Request{V: 1, Type: "request", ID: "pull", Method: "stream.pull", Params: p}))
	_, body, err := hosts.DecodeFrame(packet.Data)
	if !packet.Binary || err != nil || string(body) != "abcdef" {
		t.Fatal("resumed bytes", err, string(body))
	}
	mixed := decode[hostcmd.Session](t, c.call(t, "session.open", map[string]any{}))
	if r := request("session.resume", map[string]any{"session_id": mixed.ID, "runtime_epoch": mixed.RuntimeEpoch, "resume_token": mixed.ResumeToken, "replace_connection": true}); r.OK || r.Error.Code != "permission_denied" {
		t.Fatal("proxy reached unrestricted RTC session", r)
	}
	if r := request("session.open", map[string]any{"scope": []string{"browser"}}); r.OK {
		t.Fatal("proxy expanded scope")
	}
	call("session.close", map[string]any{"session_id": session.ID})
}
