package rtc_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"github.com/pion/webrtc/v4"
	"github.com/veypi/aic-pod/libs/hostauth"
	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/aic-pod/libs/rtc"
	hosts "github.com/veypi/aic-pod/protocol/hosts_rtc"
	rtcwire "github.com/veypi/aic-pod/protocol/hosts_rtc"
	wire "github.com/veypi/aic-pod/protocol/tool"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type rtcTools struct {
	calls atomic.Int32
}

func largeToolContent(script string) string {
	count := 400000
	if script == "large-a" {
		// Recovered command output can exceed the engine's 8 MiB capture
		// budget, but must still fit the bounded RTC response.
		count = 1500000
	}
	return strings.Repeat(script, count)
}

func (b *rtcTools) HandleTool(ctx context.Context, c wire.Caller, r wire.Request) wire.Response {
	if strings.HasPrefix(r.Exec.Script, "large-") {
		return wire.Reply(r.Protocol, r.ID, &wire.ExecResult{Content: largeToolContent(r.Exec.Script)}, nil)
	}
	if r.Exec.Script == "oversized" {
		return wire.Reply(r.Protocol, r.ID, &wire.ExecResult{Content: strings.Repeat("x", rtcwire.ToolResponseLimit), Attrs: map[string]string{
			"action": "exec", "exit_code": "0", "output": "/logs/stdout", "error_output": "/logs/stderr", "stderr": "large diagnostics",
		}}, nil)
	}
	return wire.Reply(r.Protocol, r.ID, map[string]any{"native": r.Action, "approved": c.GrantApproved, "script": r.Exec.Script, "calls": b.calls.Add(1)}, nil)
}
func (b *rtcTools) DisconnectTools(wire.Caller) {}
func TestRTCToolsWithoutBusinessSession(t *testing.T) {
	key, _ := hosts.DirectKey("secret", "host_1")
	auth, err := hostauth.NewAccess(hostauth.AccessConfig{HostID: "host_1", UserID: "owner", CredentialVersion: 1, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	defer auth.RevokeAll()
	backend := &rtcTools{}
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
	responses := make(chan json.RawMessage, 8)
	var toolResponse []byte
	var toolOffset int
	var toolChunks atomic.Int32
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		if m.IsString {
			responses <- append(json.RawMessage(nil), m.Data...)
			return
		}
		toolChunks.Add(1)
		if len(m.Data) > rtcwire.ToolResponseChunkSize {
			t.Error("tool response exceeded SCTP chunk size")
			return
		}
		data := m.Data
		if toolResponse == nil {
			if len(data) < 4 {
				t.Error("missing tool response header")
				return
			}
			size := binary.BigEndian.Uint32(data[:4])
			if size == 0 || size > rtcwire.ToolResponseLimit {
				t.Error("invalid tool response length")
				return
			}
			toolResponse = make([]byte, size)
			toolOffset = 0
			data = data[4:]
		}
		if len(data) > len(toolResponse)-toolOffset {
			t.Error("tool response fragments interleaved")
			return
		}
		toolOffset += copy(toolResponse[toolOffset:], data)
		if toolOffset == len(toolResponse) {
			responses <- toolResponse
			toolResponse = nil
		}
	})
	var signalMu sync.Mutex
	var candidates []webrtc.ICECandidateInit
	remote := false
	browserStarted := make(chan struct{}, 2)
	browserInput := make(chan []byte, 2)
	browserStopped := make(chan struct{}, 2)
	frame := []byte(`{"type":"frame","seq":73,"data":"` + string(bytes.Repeat([]byte("a"), 220000)) + `"}`)
	relay := func(ctx context.Context, caller wire.Caller, input <-chan []byte, send func([]byte) error) error {
		browserStarted <- struct{}{}
		defer func() { browserStopped <- struct{}{} }()
		if err := send(frame); err != nil {
			return err
		}
		for {
			select {
			case raw := <-input:
				if err := caller.Validate(ctx); err != nil {
					return err
				}
				browserInput <- raw
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	service, err := rtc.New(rtc.Config{Browser: relay, HostID: "host_1", Authorization: auth, Tools: backend, Send: func(sig *proto.RtcSignal) {
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

	exchange := func(value any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(value)
		if err := dc.SendText(string(raw)); err != nil {
			t.Fatal(err)
		}
		select {
		case data := <-responses:
			var r map[string]any
			if err := json.Unmarshal(data, &r); err != nil {
				t.Fatal(err)
			}
			return r
		case <-time.After(5 * time.Second):
			t.Fatal("response timeout")
			return nil
		}
	}
	call := func(script string) map[string]any {
		return exchange(rtcwire.Request{Tool: &wire.Request{Protocol: rtcwire.Protocol, ID: wire.NewID("r_"), Action: wire.ActionExec, Exec: &wire.ExecPayload{Script: script}}})
	}
	if r := call("mcp call fixture next --json"); r["error"] == nil {
		t.Fatal("unauthenticated call admitted")
	}
	denied, err := pc.CreateDataChannel(rtcwire.BrowserChannel, nil)
	if err != nil {
		t.Fatal(err)
	}
	deniedClosed := make(chan struct{})
	denied.OnClose(func() { close(deniedClosed) })
	select {
	case <-deniedClosed:
	case <-time.After(3 * time.Second):
		t.Fatal("unauthenticated browser stream stayed open")
	}
	select {
	case <-browserStarted:
		t.Fatal("unauthenticated browser relay started")
	default:
	}
	ticket, err := hosts.SignTicket(key, hosts.Ticket{HostID: "host_1", UserID: "owner", CredentialVersion: 1, PCID: "pc_tools", Fingerprint: fp}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if r := exchange(rtcwire.Request{ID: "auth1", Auth: "open", Ticket: ticket}); r["error"] != nil {
		t.Fatal(r)
	}
	stream, err := pc.CreateDataChannel(rtcwire.BrowserChannel, nil)
	if err != nil {
		t.Fatal(err)
	}
	frames := make(chan []byte, 1)
	var joined []byte
	stream.OnMessage(func(m webrtc.DataChannelMessage) {
		if m.IsString {
			return
		}
		joined = append(joined, m.Data...)
		if len(joined) >= 4 && len(joined) == 4+int(binary.BigEndian.Uint32(joined[:4])) {
			frames <- joined[4:]
			joined = nil
		}
	})
	select {
	case got := <-frames:
		if !bytes.Equal(got, frame) {
			t.Fatal("fragmented upstream frame changed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fragmented browser frame timeout")
	}
	<-browserStarted
	ack := `{"type":"ack","seq":73}`
	if err := stream.SendText(ack); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-browserInput:
		if string(got) != ack {
			t.Fatal("renderer ACK changed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("renderer ACK not forwarded")
	}
	duplicate, _ := pc.CreateDataChannel(rtcwire.BrowserChannel, nil)
	duplicateClosed := make(chan struct{})
	duplicate.OnClose(func() { close(duplicateClosed) })
	select {
	case <-duplicateClosed:
	case <-time.After(3 * time.Second):
		t.Fatal("duplicate browser stream accepted")
	}
	// Commands continue on the same authenticated peer while the viewer is open.
	for _, want := range []float64{1, 2} {
		r := call("mcp call fixture next --json")
		if r["error"] != nil {
			t.Fatal(r)
		}
		out := r["result"].(map[string]any)
		if out["calls"] != want || out["approved"] != false || out["script"] != "mcp call fixture next --json" {
			t.Fatal(out)
		}
	}
	if r := exchange(rtcwire.Request{ID: "auth2", Auth: "open", Ticket: ticket}); r["error"] == nil {
		t.Fatal("duplicate authentication admitted")
	}
	for _, approved := range []bool{true, false} {
		r := exchange(rtcwire.Request{Tool: &wire.Request{Protocol: rtcwire.Protocol, ID: "native", Action: wire.ActionExec, Exec: &wire.ExecPayload{Script: "pwd"}}, GrantApproved: approved})
		value, ok := r["result"].(map[string]any)
		if !ok || value["native"] != "exec" || value["approved"] != approved || r["request_id"] != "native" {
			t.Fatalf("native route failed: %+v", r)
		}
	}
	// Concurrent multi-megabyte tool results stay distinct, even while the
	// browser stream shares this peer and the buffered send queue fills.
	for _, name := range []string{"large-a", "large-b"} {
		raw, _ := json.Marshal(rtcwire.Request{Tool: &wire.Request{Protocol: rtcwire.Protocol, ID: name, Action: wire.ActionExec, Exec: &wire.ExecPayload{Script: name}}})
		if err := dc.SendText(string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for range 2 {
		select {
		case raw := <-responses:
			var response struct {
				ID     string `json:"request_id"`
				Result struct {
					Content string `json:"content"`
				} `json:"result"`
			}
			if err := json.Unmarshal(raw, &response); err != nil {
				t.Fatalf("fragmented response is invalid JSON: %v", err)
			}
			if seen[response.ID] || (response.ID != "large-a" && response.ID != "large-b") || response.Result.Content != largeToolContent(response.ID) {
				t.Fatalf("concurrent response %q was corrupted or duplicated", response.ID)
			}
			seen[response.ID] = true
		case <-time.After(10 * time.Second):
			t.Fatal("fragmented tool response timeout")
		}
	}
	if toolChunks.Load() < 2 {
		t.Fatal("large tool responses did not use binary chunks")
	}
	if r := call("oversized"); r["error"].(map[string]any)["code"] != "overloaded" {
		t.Fatal("oversized response did not return a bounded error")
	} else {
		result := r["result"].(map[string]any)
		attrs := result["attrs"].(map[string]any)
		if result["content"] != "" || attrs["output"] != "/logs/stdout" || attrs["error_output"] != "/logs/stderr" || attrs["exit_code"] != "0" || attrs["stderr"] != nil {
			t.Fatal("oversized response must keep log metadata without large content")
		}
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-browserStopped:
	case <-time.After(3 * time.Second):
		t.Fatal("browser relay leaked after viewer close")
	}
	if r := call("pwd"); r["error"] != nil {
		t.Fatal("viewer close terminated command connection", r)
	}
	auth.RevokeAll()
	if r := call("mcp call fixture next --json"); r["error"] == nil {
		t.Fatal("revoked authorization admitted")
	}
}
