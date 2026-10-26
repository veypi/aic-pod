package vtools

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/veypi/aic-pod/protocol"
	"github.com/veypi/vsh/commands"
)

func TestRegisterWithStreamUsesExplicitRoute(t *testing.T) {
	r := NewRegistry(commands.NewRegistry(), Options{})
	p := &streamProvider{channel: "aic-provider-default"}
	called := false
	s := StreamerFunc{
		Channel: "aic-from-manifest",
		Relay: func(context.Context, protocol.Caller, <-chan []byte, func([]byte) error) error {
			called = true
			return nil
		},
	}
	if err := r.RegisterWithStream("fixture", p, s); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.StreamHandler(p.channel); ok {
		t.Fatal("provider default channel leaked despite explicit manifest route")
	}
	relay, ok := r.StreamHandler(s.Channel)
	if !ok {
		t.Fatal("explicit stream route is not registered")
	}
	if err := relay(context.Background(), protocol.Caller{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("route did not reach the explicit streamer")
	}
	if label, ok := r.ServiceStreamChannel("fixture"); !ok || label != s.Channel {
		t.Fatalf("service stream = %q, %v; want %q", label, ok, s.Channel)
	}
}

func TestStreamChannelValidation(t *testing.T) {
	r := NewRegistry(commands.NewRegistry(), Options{})
	invalid := []string{
		"",
		" aic-leading",
		"aic-trailing ",
		"aic-control\n",
		protocol.RtcChannel,
		strings.Repeat("x", 65536),
		string([]byte{0xff}),
	}
	for _, label := range invalid {
		if err := r.RegisterWithStream("invalid", &fakeProvider{}, &fakeStreamer{channel: label}); err == nil {
			t.Fatalf("invalid channel accepted: %q", label)
		}
		if r.Has("invalid") {
			t.Fatalf("invalid channel left service registered: %q", label)
		}
	}
	if err := r.Register("automatic", &streamProvider{channel: protocol.RtcChannel}); err == nil {
		t.Fatal("provider streamer was allowed to reserve aic-tools")
	}
}

func TestRegistryConcurrentStreamChannelCollision(t *testing.T) {
	r := NewRegistry(commands.NewRegistry(), Options{})
	const count = 16
	start := make(chan struct{})
	var wg sync.WaitGroup
	var successes atomic.Int32
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			alias := fmt.Sprintf("stream-%d", i)
			p := &streamProvider{channel: "aic-shared"}
			if err := r.Register(alias, p); err == nil {
				successes.Add(1)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if got := successes.Load(); got != 1 {
		t.Fatalf("successful registrations = %d, want exactly one", got)
	}
	if aliases := r.Aliases(); len(aliases) != 1 {
		t.Fatalf("rejected concurrent registrations left services: %v", aliases)
	}
	if _, ok := r.StreamHandler("aic-shared"); !ok {
		t.Fatal("winning registration lost its stream route")
	}
}
