package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	tool "github.com/veypi/aic-pod/libs/hosts_tool"
)

func TestChromeTopEdgeFramesLive(t *testing.T) {
	if os.Getenv("AIC_BROWSER_TEST") == "" {
		t.Skip("set AIC_BROWSER_TEST=1")
	}
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><style>html{background:white}body{margin:0;height:3000px;background:#123456}header{height:20px;background:#ee1122}i{position:fixed;right:0;bottom:0;width:4px;height:4px;background:black;animation:pulse .1s steps(2,end) infinite}@keyframes pulse{to{background:white}}</style><header></header><i></i>`))
	}))
	defer fixture.Close()
	s := New(Config{StateDir: t.TempDir(), Width: 600, Height: 400})
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := tool.Caller{Subject: "owner", ConnectionID: "rtc:edge", Level: 3, ExpiresAt: time.Now().Add(time.Minute)}
	info, err := s.Create(ctx, c, CreateArgs{URL: fixture.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Wait(ctx, c, WaitArgs{PageID: info.ID, Load: true}); err != nil {
		t.Fatal(err)
	}
	p, _ := s.get(c, info.ID)
	frames, err := s.Frames(ctx, c, PageArgs{PageID: info.ID})
	if err != nil {
		t.Fatal(err)
	}
	defer frames.Close()
	input, err := s.Input(ctx, c, PageArgs{PageID: info.ID})
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	type sample struct {
		top int
		err error
	}
	samples := make(chan sample, 256)
	readCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		var data []byte
		for {
			raw, err := frames.Recv(readCtx)
			if err != nil {
				return
			}
			packet := readFramePacket(t, raw)
			if len(packet.Metadata) > 0 {
				data = nil
			}
			data = append(data, packet.Data...)
			if !packet.Final {
				continue
			}
			img, err := jpeg.Decode(bytes.NewReader(data))
			if err != nil {
				samples <- sample{err: err}
				return
			}
			top := -1
			for y := 0; y < img.Bounds().Dy(); y++ {
				r, g, b, _ := img.At(img.Bounds().Dx()/2, y).RGBA()
				if r > 50000 && g < 15000 && b < 15000 {
					top = y
					break
				}
			}
			select {
			case samples <- sample{top: top}:
			case <-readCtx.Done():
				return
			}
		}
	}()
	select {
	case first := <-samples:
		if first.err != nil || first.top != 0 {
			t.Fatalf("initial marker: %+v", first)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for n := 1; n <= 45; n++ {
		raw, _ := json.Marshal(inputBatch{Seq: uint64(n), Document: p.snapshot().Document, Events: []inputEvent{{Type: "wheel", X: 300, Y: 200, DeltaY: -120}}})
		if err := input.Send(ctx, raw); err != nil {
			t.Fatal(err)
		}
		time.Sleep(16 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)
	stop()
	<-done
	offsets := []int{}
	for len(samples) > 0 {
		v := <-samples
		if v.err != nil {
			t.Fatal(v.err)
		}
		offsets = append(offsets, v.top)
	}
	if len(offsets) < 5 {
		t.Fatalf("insufficient compositor frames: %v", offsets)
	}
	t.Logf("top marker rows: %v", offsets)
	for _, top := range offsets {
		if top != 0 {
			t.Fatalf("top edge moved during upward wheel at scroll limit: %v", offsets)
		}
	}
}
