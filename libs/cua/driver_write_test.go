package cua

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

type observedPipe struct {
	io.WriteCloser
	started chan struct{}
	once    sync.Once
}

func (p *observedPipe) Write(data []byte) (int, error) {
	p.once.Do(func() { close(p.started) })
	return p.WriteCloser.Write(data)
}

func TestBlockedDriverWriteCanBeCancelledAndClosed(t *testing.T) {
	for _, closeDriver := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "close"}[closeDriver], func(t *testing.T) {
			read, write, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer read.Close()
			defer write.Close()
			pipe := &observedPipe{WriteCloser: write, started: make(chan struct{})}
			m := newCuaMcp("fixture", t.Logf)
			m.cmd, m.stdin, m.alive = &exec.Cmd{}, pipe, true
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { _, err := m.request(ctx, "fixture", strings.Repeat("x", 2<<20)); result <- err }()
			select {
			case <-pipe.started:
			case <-time.After(time.Second):
				t.Fatal("write did not start")
			}
			closed := make(chan struct{})
			go func() {
				if closeDriver {
					m.mu.Lock()
					m.killLocked()
					m.mu.Unlock()
				} else {
					cancel()
				}
				close(closed)
			}()
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("blocked stdin held the state lock")
			}
			select {
			case err := <-result:
				if err == nil || (!closeDriver && !errors.Is(err, context.Canceled)) {
					t.Fatal("wrong interrupted-write outcome", err)
				}
			case <-time.After(time.Second):
				t.Fatal("blocked write ignored cancellation/close")
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.cmd != nil || m.stdin != nil || len(m.pending) != 0 {
				t.Fatal("partial message left a reusable transport")
			}
		})
	}
}
