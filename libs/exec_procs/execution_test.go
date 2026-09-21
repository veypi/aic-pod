package exec_procs

import (
	"context"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestExecutionDedupRetentionAndExplicitOutput(t *testing.T) {
	for _, keep := range []bool{false, true} {
		t.Run(map[bool]string{false: "managed", true: "explicit"}[keep], func(t *testing.T) {
			m := NewManager(time.Minute)
			var calls atomic.Int32
			o := CallOptions{ID: "once", Owner: "owner", Digest: "same", Command: "fixture wait", LogPath: filepath.Join(t.TempDir(), "out.log"), KeepOutput: keep, Run: func(context.Context, io.Writer) (any, error) { calls.Add(1); return map[string]int{"answer": 42}, nil }}
			r, err := m.StartCall(context.Background(), o)
			if err != nil || r.Status != "succeeded" || r.ProcessExit != nil {
				t.Fatalf("%+v %v", r, err)
			}
			if _, err = m.StartCall(context.Background(), o); err != nil || calls.Load() != 1 {
				t.Fatal("duplicate executed", err)
			}
			changed := o
			changed.Digest = "different"
			if _, err = m.StartCall(context.Background(), changed); wire.AsFault(err).Code != "conflict" {
				t.Fatal(err)
			}
			e := m.Get(o.ID)
			e.mu.Lock()
			e.completed = time.Now().Add(-CompletedTTL - time.Second)
			e.mu.Unlock()
			if m.Get(o.ID) != nil {
				t.Fatal("completed entry retained past TTL")
			}
			_, err = os.Stat(o.LogPath)
			if (err == nil) != keep {
				t.Fatal("wrong output retention", err)
			}
			if _, err = m.StartCall(context.Background(), o); wire.AsFault(err).Code != "expired" || calls.Load() != 1 {
				t.Fatal("expired ID reran", err)
			}
		})
	}
}
func TestCancellationWaitsForHandlerAndOutputIsBounded(t *testing.T) {
	m := NewManager(time.Minute)
	started, release := make(chan struct{}), make(chan struct{})
	wait, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	r, err := m.StartCall(wait, CallOptions{ID: "wait", LogPath: filepath.Join(t.TempDir(), "wait.log"), Run: func(ctx context.Context, out io.Writer) (any, error) {
		close(started)
		<-ctx.Done()
		<-release
		return nil, ctx.Err()
	}})
	if err != nil || !r.Background {
		t.Fatal(r, err)
	}
	<-started
	if err = m.Kill(r.ID); err != nil {
		t.Fatal(err)
	}
	if e := m.Get(r.ID); e.Done() || e.Status() != "cancelling" {
		t.Fatal("cancelled before handler exited")
	}
	close(release)
	done, err := m.Wait(context.Background(), r.ID, time.Second)
	if err != nil || done.Status != "cancelled" || done.Background {
		t.Fatal(done, err)
	}
	big, err := m.StartCall(context.Background(), CallOptions{ID: "big", LogPath: filepath.Join(t.TempDir(), "big.log"), Run: func(ctx context.Context, out io.Writer) (any, error) {
		_, err := io.Copy(out, strings.NewReader(strings.Repeat("x\n", MaxOutputBytes)))
		return nil, err
	}})
	if err != nil || !big.Truncated || len(big.Content) > MaxPreviewBytes {
		t.Fatal(big, err)
	}
	info, err := os.Stat(big.LogPath)
	if err != nil || info.Size() != MaxOutputBytes {
		t.Fatal(info, err)
	}
}
