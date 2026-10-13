package execution

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	vshnet "github.com/veypi/vsh/network"
)

// TestNetClientStreamsResponseToSink：req.Sink 非 nil 时响应体边收边写、Body 留空，
// OnResponse 先于正文回调（curl -i 的头块依赖它）。
func TestNetClientStreamsResponseToSink(t *testing.T) {
	t.Parallel()

	payload := strings.Repeat("stream-", 200000) // ~1.4 MiB
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.WriteString(w, payload)
	}))
	defer server.Close()

	c := NewNetClient(NetClientConfig{Resolver: fakeResolver{}, DialContext: hijackDial(server)})

	var sink bytes.Buffer
	var (
		gotCode    int
		gotHeaders map[string]string
	)
	resp, err := c.Do(context.Background(), &vshnet.Request{
		Method: "GET",
		URL:    "http://example.com/big.bin",
		Sink:   &sink,
		OnResponse: func(statusCode int, status string, headers map[string]string) {
			gotCode, gotHeaders = statusCode, headers
		},
	})
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	if !resp.Streamed || len(resp.Body) != 0 {
		t.Fatalf("Streamed = %v, Body = %d bytes; want streamed with empty Body", resp.Streamed, len(resp.Body))
	}
	if got, want := resp.BytesWritten, int64(len(payload)); got != want {
		t.Fatalf("BytesWritten = %d, want %d", got, want)
	}
	if got := sink.String(); got != payload {
		t.Fatalf("sink = %d bytes, want %d payload bytes", len(got), len(payload))
	}
	if gotCode != http.StatusOK || gotHeaders["Content-Type"] != "application/octet-stream" {
		t.Fatalf("OnResponse meta = %d %v", gotCode, gotHeaders)
	}
}

// TestNetClientStreamUnlimitedByDefault：MaxResponseBytes 缺省（0）= 不限制——
// 超过旧 64 MiB 默认上限的大文件流式下载必须成功（设备端下载大文件的路径）。
func TestNetClientStreamUnlimitedByDefault(t *testing.T) {
	t.Parallel()

	const payloadSize = 70 << 20 // > 旧 DefaultNetMaxResponseBytes (64 MiB)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		block := bytes.Repeat([]byte("b"), 64<<10)
		for sent := 0; sent < payloadSize; sent += len(block) {
			if _, err := w.Write(block); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	c := NewNetClient(NetClientConfig{Resolver: fakeResolver{}, DialContext: hijackDial(server)})

	resp, err := c.Do(context.Background(), &vshnet.Request{
		Method: "GET",
		URL:    "http://example.com/huge.bin",
		Sink:   io.Discard,
	})
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	if !resp.Streamed || resp.BytesWritten != payloadSize {
		t.Fatalf("Streamed = %v, BytesWritten = %d, want %d", resp.Streamed, resp.BytesWritten, payloadSize)
	}
}

// TestNetClientStreamRespectsConfiguredCap：上限是引入方的配置项——显式设限后
// 流式路径同样在跨线处中止（cloud 侧 64 MiB 即走这条）。
func TestNetClientStreamRespectsConfiguredCap(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), 4096))
	}))
	defer server.Close()

	c := NewNetClient(NetClientConfig{
		MaxResponseBytes: 1024,
		Resolver:         fakeResolver{},
		DialContext:      hijackDial(server),
	})

	var sink bytes.Buffer
	_, err := c.Do(context.Background(), &vshnet.Request{
		Method: "GET",
		URL:    "http://example.com/big.bin",
		Sink:   &sink,
	})
	var tooLarge *vshnet.ResponseTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("oversize streamed response = %v, want ResponseTooLargeError", err)
	}
	if sink.Len() > 1024 {
		t.Fatalf("sink = %d bytes, want at most the 1024-byte cap", sink.Len())
	}
}
