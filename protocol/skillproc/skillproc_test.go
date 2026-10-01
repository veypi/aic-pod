package skillproc

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"sync"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		h       Header
		payload []byte
	}{
		{"invoke with stdin", Header{ID: "r1", Type: TypeInvoke, Argv: []string{"echo", "hi"}, Cwd: "/tmp", Env: map[string]string{"A": "b"}}, []byte("stdin-bytes")},
		{"stdout frame", Header{ID: "r1", Type: TypeFrame, Stream: StreamStdout}, bytes.Repeat([]byte("x"), 10000)},
		{"exit no payload", Header{ID: "r1", Type: TypeExit, Code: 3}, nil},
		{"stream open", Header{ID: "s1", Type: TypeStreamOpen, Name: "echo"}, nil},
		{"stream frame binary", Header{ID: "s1", Type: TypeStreamFrame}, []byte{0, 1, 2, 255, 254}},
		{"cancel", Header{ID: "r1", Type: TypeCancel}, nil},
		{"error", Header{ID: "r9", Type: TypeError, Error: "no such method"}, nil},
		{"unicode argv", Header{ID: "r2", Type: TypeInvoke, Argv: []string{"写", "文件"}}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteFrame(&buf, c.h, c.payload); err != nil {
				t.Fatal(err)
			}
			h, payload, err := ReadFrame(&buf)
			if err != nil {
				t.Fatal(err)
			}
			if h.V != 1 || h.ID != c.h.ID || h.Type != c.h.Type || h.Stream != c.h.Stream ||
				h.Code != c.h.Code || h.Error != c.h.Error || h.Name != c.h.Name || h.Cwd != c.h.Cwd {
				t.Fatalf("header mismatch: %+v != %+v", h, c.h)
			}
			if len(h.Argv) != len(c.h.Argv) {
				t.Fatalf("argv mismatch: %v != %v", h.Argv, c.h.Argv)
			}
			if !bytes.Equal(payload, c.payload) && !(payload == nil && len(c.payload) == 0) {
				t.Fatalf("payload mismatch: %d != %d bytes", len(payload), len(c.payload))
			}
		})
	}
}

func TestMultiFrameStream(t *testing.T) {
	var buf bytes.Buffer
	// 连续多帧（模拟 stdout 多次刷写 + exit 收尾）不出帧间串扰
	for _, p := range [][]byte{[]byte("a"), bytes.Repeat([]byte("b"), 4096), nil} {
		if err := WriteFrame(&buf, Header{ID: "r", Type: TypeFrame, Stream: StreamStdout}, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := WriteFrame(&buf, Header{ID: "r", Type: TypeExit, Code: 0}, nil); err != nil {
		t.Fatal(err)
	}
	var got []byte
	for {
		h, p, err := ReadFrame(&buf)
		if err != nil {
			t.Fatal(err)
		}
		if h.Type == TypeExit {
			break
		}
		got = append(got, p...)
	}
	if want := append([]byte("a"), bytes.Repeat([]byte("b"), 4096)...); !bytes.Equal(got, want) {
		t.Fatalf("stream payload = %d bytes, want %d", len(got), len(want))
	}
}

func TestBadHeaderRejected(t *testing.T) {
	// 头长越界
	if _, _, err := ReadFrame(bytes.NewReader([]byte{0xff, 0xff, 0xff, 0xff})); err == nil {
		t.Fatal("oversized header length must be rejected")
	}
	// 非法 JSON
	var buf bytes.Buffer
	buf.Write([]byte{0, 0, 0, 5})
	buf.WriteString("{bad")
	if _, _, err := ReadFrame(&buf); err == nil {
		t.Fatal("malformed header must be rejected")
	}
	// 负载长越界（直接构造头 JSON——WriteFrame 会覆写 Len）
	hb, _ := json.Marshal(Header{ID: "x", Type: TypeFrame, Len: MaxPayload + 1})
	var buf2 bytes.Buffer
	buf2.Write([]byte{0, 0, 0, byte(len(hb))})
	buf2.Write(hb)
	if _, _, err := ReadFrame(&buf2); err == nil {
		t.Fatal("oversized payload length must be rejected")
	}
}

func TestConnConcurrentSend(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	ca, cb := NewConn(a), NewConn(b)

	var wg sync.WaitGroup
	const writers = 8
	const frames = 50
	wg.Add(writers)
	for w := 0; w < writers; w++ {
		go func(id byte) {
			defer wg.Done()
			payload := bytes.Repeat([]byte{id}, 100)
			for i := 0; i < frames; i++ {
				if err := ca.Send(Header{ID: "r", Type: TypeFrame, Stream: StreamStdout}, payload); err != nil {
					t.Error(err)
					return
				}
			}
		}(byte('a' + w))
	}
	// 单读者收齐全部帧；帧完整性 = 每帧负载同质（交叠会产生混合字节）
	counts := map[byte]int{}
	for i := 0; i < writers*frames; i++ {
		_, p, err := cb.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if len(p) != 100 {
			t.Fatalf("payload len = %d, want 100", len(p))
		}
		for _, x := range p[1:] {
			if x != p[0] {
				t.Fatal("frame interleave detected")
			}
		}
		counts[p[0]]++
	}
	wg.Wait()
	if len(counts) != writers {
		t.Fatalf("received frames from %d writers, want %d", len(counts), writers)
	}
}

func TestCleanEOF(t *testing.T) {
	a, b := net.Pipe()
	ca, cb := NewConn(a), NewConn(b)
	// net.Pipe 无缓冲：写端阻塞直到读端接收，Send 必须异步
	done := make(chan error, 1)
	go func() {
		done <- ca.Send(Header{ID: "r", Type: TypeExit, Code: 0}, nil)
		ca.Close()
	}()
	if _, _, err := cb.Recv(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, _, err := cb.Recv(); err != io.EOF {
		t.Fatalf("closed conn Recv = %v, want io.EOF", err)
	}
}
