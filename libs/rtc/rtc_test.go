package rtc

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/aic-pod/libs/vcore"
)

// 环路对测：进程内 pion offerer（模拟页面）+ Service 应答方，走真实
// UDP/DataChannel 全链路——信令路由、鉴权帧（正/误）、内联结果、
// 流式结果（>inlineLimit 重组）、流式写（chunk 重组注回 content）、
// readbin/writebin 二进制通道对测。

// testClient 是页面侧的最小帧客户端。
type testClient struct {
	pc *webrtc.PeerConnection
	dc *webrtc.DataChannel

	mu       sync.Mutex
	waiters  map[string]chan *fsFrame // 终态帧投递（result/auth_ok/auth_err）
	streams  map[string]*strings.Builder
	heads    map[string]*fsFrame // 流式响应的 head 帧（attrs/bin 标记经 end 带出）
	files    map[string][]byte   // readbin 桩的文件集
	lastAuth *fsFrame
}

func newTestClient() *testClient {
	return &testClient{
		waiters: map[string]chan *fsFrame{},
		streams: map[string]*strings.Builder{},
		heads:   map[string]*fsFrame{},
		files:   map[string][]byte{},
	}
}

// putFile 向 readbin 桩文件集写入（测试主 goroutine 与 executeBin 并发，走 mu）。
func (tc *testClient) putFile(path string, data []byte) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.files[path] = data
}

// getFile 取（writebin 桩写入后的）文件字节副本。
func (tc *testClient) getFile(path string) []byte {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	return append([]byte(nil), tc.files[path]...)
}

// serveWriteBin 是注入 Service 的 writebin 桩：把解码后的字节存入 files
// （写盘语义由 vcore.WriteBin 用例覆盖，通道层只验载荷精确与路由）。
func (tc *testClient) serveWriteBin(path string, data []byte) (int, error) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.files[path] = append([]byte(nil), data...)
	return len(data), nil
}

// serveReadBin 是注入 Service 的 readbin 桩：从 files 按区间取字节（复刻
// vcore.ReadBin 的区间语义——通道层对测不管 vcore 语义，那有 vcore 自己的用例）。
func (tc *testClient) serveReadBin(path string, off, length int64) ([]byte, string, int64, error) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	data, ok := tc.files[path]
	if !ok {
		return nil, "", 0, fmt.Errorf("readbin: %s: no such file", path)
	}
	total := int64(len(data))
	if off > total {
		return nil, "", 0, fmt.Errorf("readbin: off %d exceeds file size %d", off, total)
	}
	if length <= 0 || off+length > total {
		length = total - off
	}
	return data[off : off+length], "application/octet-stream", total, nil
}

// setup 创建信令直路由的测试服务并完成 RTC 建连（offer/answer/trickle 全走
// 内存直路由，DataChannel 为真实 UDP/DTLS 链路）。返回服务本体（供连接回收
// 类用例断言内部状态）与页面侧最小帧客户端。
func setup(t *testing.T, code string, runFS func(context.Context, json.RawMessage) (*vcore.Result, error)) (*Service, *testClient) {
	t.Helper()
	tc := newTestClient()
	svc, err := New(Config{
		Code: code, HostID: "host_test", Hostname: "testbox", Version: "v0.0.0-test",
		Send:  func(*proto.RtcSignal) {}, // 建连前由下方回调替换为直路由
		RunFS: runFS, ReadBin: tc.serveReadBin, WriteBin: tc.serveWriteBin, Logf: func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("rtc New: %v", err)
	}
	t.Cleanup(svc.Close)

	pc, err := webrtc.NewAPI().NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("client pc: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	tc.pc = pc

	pcID := "pc1"
	answered := make(chan struct{})
	var once sync.Once
	// 服务端出向信令 → 客户端直路由
	svc.cfg.Send = func(sig *proto.RtcSignal) {
		switch sig.Kind {
		case proto.RtcAnswer:
			if err := pc.SetRemoteDescription(webrtc.SessionDescription{
				Type: webrtc.SDPTypeAnswer, SDP: sig.SDP,
			}); err == nil {
				once.Do(func() { close(answered) })
			}
		case proto.RtcCandidate:
			var init webrtc.ICECandidateInit
			if json.Unmarshal([]byte(sig.Candidate), &init) == nil {
				_ = pc.AddICECandidate(init)
			}
		}
	}
	// 客户端候选 → 服务端（pion 在 remote 描述缺失时拒 add，故仅在 answer 后转发；
	// answer 前的候选由 ICE 重启覆盖——测试拓扑下候选在 offer 后持续产出，够用）
	var remoteSet atomic.Bool
	go func() {
		<-answered
		remoteSet.Store(true)
	}()
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil || !remoteSet.Load() {
			return
		}
		data, _ := json.Marshal(c.ToJSON())
		svc.HandleSignal(&proto.RtcSignal{PC: pcID, Kind: proto.RtcCandidate, Candidate: string(data)})
	})

	dc, err := pc.CreateDataChannel(fsChannelLabel, nil)
	if err != nil {
		t.Fatalf("create dc: %v", err)
	}
	tc.dc = dc
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if !msg.IsString {
			return
		}
		var f fsFrame
		if json.Unmarshal(msg.Data, &f) != nil {
			return
		}
		if f.Op == "auth_ok" || f.Op == "auth_err" {
			tc.mu.Lock()
			tc.lastAuth = &f
			tc.mu.Unlock()
		}
		tc.onFrame(&f)
	})
	opened := make(chan struct{})
	dc.OnOpen(func() { close(opened) })

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("offer: %v", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local: %v", err)
	}
	go svc.HandleSignal(&proto.RtcSignal{PC: pcID, Kind: proto.RtcOffer, SDP: pc.LocalDescription().SDP})

	select {
	case <-answered:
	case <-time.After(10 * time.Second):
		t.Fatal("answer timeout")
	}
	remoteSet.Store(true)
	select {
	case <-opened:
	case <-time.After(10 * time.Second):
		t.Fatal("dc open timeout")
	}
	return svc, tc
}

func (tc *testClient) onFrame(f *fsFrame) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	switch f.Op {
	case "chunk":
		if sb, ok := tc.streams[f.ID]; ok {
			sb.WriteString(f.Text)
		}
	case "end":
		if sb, ok := tc.streams[f.ID]; ok {
			delete(tc.streams, f.ID)
			head := tc.heads[f.ID]
			delete(tc.heads, f.ID)
			if ch, ok := tc.waiters[f.ID]; ok {
				out := &fsFrame{ID: f.ID, Op: "result", OK: true, Data: json.RawMessage(sb.String())}
				if head != nil {
					out.Bin = head.Bin
					out.Attrs = head.Attrs
				}
				ch <- out
			}
		}
	default: // result / auth_ok / auth_err
		if f.Op == "result" && f.Stream {
			tc.streams[f.ID] = &strings.Builder{}
			tc.heads[f.ID] = f
			return
		}
		if ch, ok := tc.waiters[f.ID]; ok {
			ch <- f
		}
	}
}

func (tc *testClient) register(id string) chan *fsFrame {
	ch := make(chan *fsFrame, 1)
	tc.mu.Lock()
	tc.waiters[id] = ch
	tc.mu.Unlock()
	return ch
}

func (tc *testClient) sendFrame(t *testing.T, raw []byte) {
	t.Helper()
	if err := tc.dc.SendText(string(raw)); err != nil {
		t.Fatalf("send: %v", err)
	}
}

func (tc *testClient) await(t *testing.T, ch chan *fsFrame) *fsFrame {
	t.Helper()
	select {
	case f := <-ch:
		return f
	case <-time.After(10 * time.Second):
		t.Fatal("response timeout")
		return nil
	}
}

// auth 发鉴权帧并等回执。
func (tc *testClient) auth(t *testing.T, code string) *fsFrame {
	t.Helper()
	ch := tc.register("a1")
	raw, _ := json.Marshal(fsFrame{ID: "a1", Op: "auth", Code: code})
	tc.sendFrame(t, raw)
	return tc.await(t, ch)
}

// call 发 fs 请求（args 即 RunFS 原生输入含 action；content 超 inlineLimit
// 自动剥离走 chunk 流，设备端重组注回）并等待结果载荷。
func (tc *testClient) call(t *testing.T, id, action string, args map[string]any) *vcore.Result {
	t.Helper()
	args["action"] = action
	head := map[string]any{"id": id, "op": "fs"}
	content, _ := args["content"].(string)
	stream := len(content) > frameInlineLimit
	if stream {
		delete(args, "content")
	}
	rawArgs, _ := json.Marshal(args)
	head["args"] = json.RawMessage(rawArgs)
	if stream {
		head["stream"] = true
	}
	ch := tc.register(id)
	raw, _ := json.Marshal(head)
	tc.sendFrame(t, raw)
	if stream {
		for seq, off := 0, 0; off < len(content); seq, off = seq+1, off+frameChunkSize {
			end := off + frameChunkSize
			if end > len(content) {
				end = len(content)
			}
			cf, _ := json.Marshal(fsFrame{ID: id, Op: "chunk", Seq: seq, Text: content[off:end]})
			tc.sendFrame(t, cf)
		}
		ef, _ := json.Marshal(fsFrame{ID: id, Op: "end"})
		tc.sendFrame(t, ef)
	}
	resp := tc.await(t, ch)
	if !resp.OK {
		t.Fatalf("call %s failed: %s", action, resp.Error)
	}
	// 线上契约断言（2026-09-10 实网事故）：result 载荷必须含小写 "content" 键
	//（vcore.Result json tag）——页面端按小写解析，而 Go 反序列化大小写不敏感，
	// 没有本断言时 mock 漂移（JS 假设备用小写、Go 对测用大写无感）无法被拦住。
	if !strings.Contains(string(resp.Data), `"content"`) {
		t.Fatalf("wire contract broken: result payload must carry lowercase \"content\" key, got: %s", resp.Data)
	}
	var res vcore.Result
	if err := json.Unmarshal(resp.Data, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	return &res
}

// stubRunFS 协议层桩执行体（fs 语义由 vcore 测试覆盖，这里只验证通道）：
// ls 回固定 JSON；read 回小文本或 64KB 大文本（big=true）；write 回显 content 长度。
func stubRunFS(big bool) func(context.Context, json.RawMessage) (*vcore.Result, error) {
	return func(_ context.Context, raw json.RawMessage) (*vcore.Result, error) {
		var p struct {
			Action  string `json:"action"`
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		switch p.Action {
		case "ls":
			return &vcore.Result{Content: fmt.Sprintf(`{"cwd":"%s","items":[]}`, p.Path)}, nil
		case "read":
			content := "hello " + p.Path
			if big {
				content = strings.Repeat("0123456789abcdef", 4096) // 64KB，强制流式
			}
			return &vcore.Result{Content: content, Attrs: map[string]string{"mime": "text/plain"}}, nil
		case "write":
			return &vcore.Result{Content: "ok",
				Attrs: map[string]string{"echo_len": fmt.Sprint(len(p.Content))}}, nil
		}
		return nil, fmt.Errorf("unknown action %q", p.Action)
	}
}

func TestRTCAuthWrongCode(t *testing.T) {
	_, tc := setup(t, "secret-code", stubRunFS(false))
	if f := tc.auth(t, "wrong"); f.Op != "auth_err" {
		t.Fatalf("wrong code: got op=%q", f.Op)
	}
}

func TestRTCAuthAndInlineCall(t *testing.T) {
	_, tc := setup(t, "secret-code", stubRunFS(false))
	f := tc.auth(t, "secret-code")
	if f.Op != "auth_ok" || f.HostID != "host_test" || f.Hostname != "testbox" {
		t.Fatalf("auth_ok: %+v", f)
	}
	res := tc.call(t, "r1", "ls", map[string]any{"path": "/tmp"})
	if !strings.Contains(res.Content, `"cwd":"/tmp"`) {
		t.Fatalf("ls result: %s", res.Content)
	}
}

func TestRTCStreamedResult(t *testing.T) {
	_, tc := setup(t, "secret-code", stubRunFS(true))
	tc.auth(t, "secret-code")
	res := tc.call(t, "r2", "read", map[string]any{"path": "/big.txt"})
	want := strings.Repeat("0123456789abcdef", 4096)
	if res.Content != want {
		t.Fatalf("streamed result mismatch: got %d bytes want %d", len(res.Content), len(want))
	}
	if res.Attrs["mime"] != "text/plain" {
		t.Fatalf("attrs lost: %+v", res.Attrs)
	}
}

func TestRTCStreamedWrite(t *testing.T) {
	_, tc := setup(t, "secret-code", stubRunFS(true))
	tc.auth(t, "secret-code")
	big := strings.Repeat("xyz\n", 20000) // 80KB 写内容，走 chunk 流注回
	res := tc.call(t, "r3", "write", map[string]any{"path": "/a/b.txt", "content": big})
	if res.Attrs["echo_len"] != fmt.Sprint(len(big)) {
		t.Fatalf("write echo_len=%s want %d", res.Attrs["echo_len"], len(big))
	}
}

// fs 通道关闭 → PC 立即回收（2026-09-10 评审 P2：未 authed 的连接此前只关 dc，
// PC 滞留 pcs 永不回收，信令面洪泛可耗尽资源）。
func TestRTCChannelCloseReclaimsPC(t *testing.T) {
	svc, tc := setup(t, "secret-code", stubRunFS(false))
	if svc.getPC("pc1") == nil {
		t.Fatal("pc not registered after offer")
	}
	_ = tc.dc.Close() // 客户端主动关 fs 通道（未鉴权状态）
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if svc.getPC("pc1") == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("pc not reclaimed after fs channel close")
}

// ---- readbin 通道对测（2026-09-10）：内联/流式 base64 精确往返、区间透传、错误帧 ----

// callBin 发 readbin 请求并收载荷：返回 (解码字节, attrs)；流式时 attrs 取自 head 帧。
func (tc *testClient) callBin(t *testing.T, id, path string, off, length int64) ([]byte, map[string]string) {
	t.Helper()
	ch := tc.register(id)
	rawArgs, _ := json.Marshal(map[string]any{"path": path, "off": off, "len": length})
	raw, _ := json.Marshal(fsFrame{ID: id, Op: "readbin", Args: rawArgs})
	tc.sendFrame(t, raw)
	resp := tc.await(t, ch)
	if !resp.OK {
		t.Fatalf("readbin %s failed: %s", path, resp.Error)
	}
	b64 := resp.Text // 内联载荷
	if resp.Bin && resp.Text == "" {
		b64 = string(resp.Data) // 流式：end 重组的 base64 文本
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("bad base64 payload (%d chars): %v", len(b64), err)
	}
	return data, resp.Attrs
}

func TestRTCReadBinInline(t *testing.T) {
	_, tc := setup(t, "secret-code", stubRunFS(false))
	tc.auth(t, "secret-code")
	payload := []byte{0x00, 0x01, 0xfe, 0xff} // 含不可打印字节
	tc.putFile("/v.bin", payload)
	data, attrs := tc.callBin(t, "b1", "/v.bin", 0, 0)
	if !bytes.Equal(data, payload) {
		t.Fatalf("inline payload mismatch: %v", data)
	}
	if attrs["size"] != "4" || attrs["total"] != "4" || attrs["mime"] == "" {
		t.Fatalf("attrs: %+v", attrs)
	}
}

func TestRTCReadBinStreamed(t *testing.T) {
	_, tc := setup(t, "secret-code", stubRunFS(false))
	tc.auth(t, "secret-code")
	payload := make([]byte, 100<<10) // 100KB 全字节值循环 → base64 远超 inlineLimit
	for i := range payload {
		payload[i] = byte(i)
	}
	tc.putFile("/big.bin", payload)
	data, attrs := tc.callBin(t, "b2", "/big.bin", 0, 0)
	if !bytes.Equal(data, payload) {
		t.Fatalf("streamed payload mismatch: got %d bytes", len(data))
	}
	if attrs["size"] != fmt.Sprint(len(payload)) {
		t.Fatalf("attrs: %+v", attrs)
	}
}

func TestRTCReadBinRange(t *testing.T) {
	_, tc := setup(t, "secret-code", stubRunFS(false))
	tc.auth(t, "secret-code")
	tc.putFile("/r.bin", []byte("0123456789abcdef"))
	data, attrs := tc.callBin(t, "b3", "/r.bin", 4, 6)
	if string(data) != "456789" || attrs["size"] != "6" || attrs["total"] != "16" {
		t.Fatalf("range: %q attrs=%+v", data, attrs)
	}
}

func TestRTCReadBinError(t *testing.T) {
	_, tc := setup(t, "secret-code", stubRunFS(false))
	tc.auth(t, "secret-code")
	ch := tc.register("b4")
	rawArgs, _ := json.Marshal(map[string]any{"path": "/nope.bin"})
	raw, _ := json.Marshal(fsFrame{ID: "b4", Op: "readbin", Args: rawArgs})
	tc.sendFrame(t, raw)
	resp := tc.await(t, ch)
	if resp.OK || !strings.Contains(resp.Error, "no such file") {
		t.Fatalf("want error frame, got %+v", resp)
	}
}

// readbin 并发（2026-09-10）：同一 channel 两请求背靠背发出，设备端 goroutine
// 并发执行、chunk 帧带 id 交错回传，客户端按 id 分流重组，两路各自字节精确。
func TestRTCReadBinConcurrent(t *testing.T) {
	_, tc := setup(t, "secret-code", stubRunFS(false))
	tc.auth(t, "secret-code")
	// 两文件载荷位置相关且互异（> inlineLimit 走流式），串味立即可见
	a := make([]byte, 80<<10)
	b := make([]byte, 80<<10)
	for i := range a {
		a[i] = byte(i)
		b[i] = byte(255 - i)
	}
	tc.putFile("/a.bin", a)
	tc.putFile("/b.bin", b)
	chA := tc.register("c1")
	chB := tc.register("c2")
	send := func(id, path string) {
		rawArgs, _ := json.Marshal(map[string]any{"path": path})
		raw, _ := json.Marshal(fsFrame{ID: id, Op: "readbin", Args: rawArgs})
		tc.sendFrame(t, raw)
	}
	send("c1", "/a.bin")
	send("c2", "/b.bin") // 不等第一路完成
	respA := tc.await(t, chA)
	respB := tc.await(t, chB)
	if !respA.OK || !respB.OK {
		t.Fatalf("concurrent readbin: A ok=%v err=%s; B ok=%v err=%s", respA.OK, respA.Error, respB.OK, respB.Error)
	}
	dataA, err := base64.StdEncoding.DecodeString(string(respA.Data))
	if err != nil {
		t.Fatalf("A bad base64: %v", err)
	}
	dataB, err := base64.StdEncoding.DecodeString(string(respB.Data))
	if err != nil {
		t.Fatalf("B bad base64: %v", err)
	}
	if !bytes.Equal(dataA, a) {
		t.Fatalf("A payload mismatch: got %d bytes", len(dataA))
	}
	if !bytes.Equal(dataB, b) {
		t.Fatalf("B payload mismatch: got %d bytes", len(dataB))
	}
	if respA.Attrs["size"] != fmt.Sprint(len(a)) || respB.Attrs["size"] != fmt.Sprint(len(b)) {
		t.Fatalf("attrs: A=%+v B=%+v", respA.Attrs, respB.Attrs)
	}
}

// ---- writebin 通道对测（2026-09-12）：内联/流式 base64 精确还原、错误帧 ----

// callWriteBin 发 writebin 请求（>inlineLimit 自动走 chunk 流）并等结果帧。
func (tc *testClient) callWriteBin(t *testing.T, id, path string, payload []byte) *fsFrame {
	t.Helper()
	b64 := base64.StdEncoding.EncodeToString(payload)
	rawArgs, _ := json.Marshal(map[string]any{"path": path, "size": len(payload)})
	head := map[string]any{"id": id, "op": "writebin", "args": json.RawMessage(rawArgs)}
	stream := len(b64) > frameInlineLimit
	if stream {
		head["stream"] = true
	} else {
		head["text"] = b64
	}
	ch := tc.register(id)
	raw, _ := json.Marshal(head)
	tc.sendFrame(t, raw)
	if stream {
		for seq, off := 0, 0; off < len(b64); seq, off = seq+1, off+frameChunkSize {
			end := off + frameChunkSize
			if end > len(b64) {
				end = len(b64)
			}
			cf, _ := json.Marshal(fsFrame{ID: id, Op: "chunk", Seq: seq, Text: b64[off:end]})
			tc.sendFrame(t, cf)
		}
		ef, _ := json.Marshal(fsFrame{ID: id, Op: "end"})
		tc.sendFrame(t, ef)
	}
	return tc.await(t, ch)
}

func TestRTCWriteBinInline(t *testing.T) {
	_, tc := setup(t, "secret-code", stubRunFS(false))
	tc.auth(t, "secret-code")
	payload := []byte{0x89, 0x50, 0x4E, 0x47, 0x00, 0xFE, 0xFF} // PNG 魔数头 + 不可打印字节
	resp := tc.callWriteBin(t, "w1", "/out.png", payload)
	if !resp.OK {
		t.Fatalf("writebin failed: %s", resp.Error)
	}
	// 线上契约断言（同 call()）：result 载荷必须含小写 "content" 键
	if !strings.Contains(string(resp.Data), `"content"`) {
		t.Fatalf("wire contract broken: writebin result must carry lowercase \"content\" key, got: %s", resp.Data)
	}
	if got := tc.getFile("/out.png"); !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch: got %v want %v", got, payload)
	}
	var res vcore.Result
	if err := json.Unmarshal(resp.Data, &res); err != nil {
		t.Fatalf("unmarshal writebin result: %v", err)
	}
	if res.Attrs["bytes"] != fmt.Sprint(len(payload)) || res.Attrs["path"] != "/out.png" {
		t.Fatalf("attrs: %+v", res.Attrs)
	}
}

func TestRTCWriteBinStreamed(t *testing.T) {
	_, tc := setup(t, "secret-code", stubRunFS(false))
	tc.auth(t, "secret-code")
	payload := make([]byte, 100<<10) // 100KB 全字节值循环 → base64 远超 inlineLimit
	for i := range payload {
		payload[i] = byte(i)
	}
	resp := tc.callWriteBin(t, "w2", "/big.bin", payload)
	if !resp.OK {
		t.Fatalf("writebin failed: %s", resp.Error)
	}
	if got := tc.getFile("/big.bin"); !bytes.Equal(got, payload) {
		t.Fatalf("streamed payload mismatch: got %d bytes", len(got))
	}
}

func TestRTCWriteBinBadBase64(t *testing.T) {
	_, tc := setup(t, "secret-code", stubRunFS(false))
	tc.auth(t, "secret-code")
	ch := tc.register("w3")
	rawArgs, _ := json.Marshal(map[string]any{"path": "/x.bin"})
	raw, _ := json.Marshal(fsFrame{ID: "w3", Op: "writebin", Args: rawArgs, Text: "!!!"})
	tc.sendFrame(t, raw)
	resp := tc.await(t, ch)
	if resp.OK || !strings.Contains(resp.Error, "base64") {
		t.Fatalf("want base64 error frame, got %+v", resp)
	}
	if got := tc.getFile("/x.bin"); got != nil {
		t.Fatalf("bad payload must not write: %v", got)
	}
}

func TestRTCWriteBinInvalidArgs(t *testing.T) {
	_, tc := setup(t, "secret-code", stubRunFS(false))
	tc.auth(t, "secret-code")
	ch := tc.register("w4")
	rawArgs, _ := json.Marshal(map[string]any{})
	raw, _ := json.Marshal(fsFrame{ID: "w4", Op: "writebin", Args: rawArgs, Text: "AAAA"})
	tc.sendFrame(t, raw)
	resp := tc.await(t, ch)
	if resp.OK || !strings.Contains(resp.Error, "invalid args") {
		t.Fatalf("want invalid args frame, got %+v", resp)
	}
}
