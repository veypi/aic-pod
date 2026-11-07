//go:build darwin

package indexer

import (
	"sync"
	"time"

	"github.com/fsnotify/fsevents"
)

// fseventsIndexWatcher uses macOS' recursive, file-level event stream. The
// kqueue fsnotify backend watches individual files and can exhaust the process
// descriptor limit on an ordinary source workspace.
type fseventsIndexWatcher struct {
	stream    *fsevents.EventStream
	events    chan indexEvent
	errs      chan error
	done      chan struct{}
	closeOnce sync.Once
}

func newIndexWatcher(root string) (indexWatcher, error) {
	stream := &fsevents.EventStream{
		Paths: []string{root}, Latency: 50 * time.Millisecond,
		Flags: fsevents.FileEvents | fsevents.WatchRoot | fsevents.NoDefer,
	}
	if err := stream.Start(); err != nil {
		return nil, err
	}
	w := &fseventsIndexWatcher{
		stream: stream, events: make(chan indexEvent), errs: make(chan error), done: make(chan struct{}),
	}
	go w.run()
	return w, nil
}

// FSEvents is recursive from the configured root, so per-directory Add is a no-op.
func (w *fseventsIndexWatcher) Add(string) error          { return nil }
func (w *fseventsIndexWatcher) Events() <-chan indexEvent { return w.events }
func (w *fseventsIndexWatcher) Errors() <-chan error      { return w.errs }

func (w *fseventsIndexWatcher) Close() error {
	w.closeOnce.Do(func() {
		close(w.done)
		w.stream.Stop()
	})
	return nil
}

func (w *fseventsIndexWatcher) run() {
	defer close(w.events)
	defer close(w.errs)
	for {
		select {
		case <-w.done:
			return
		case batch, ok := <-w.stream.Events:
			if !ok {
				return
			}
			for _, event := range batch {
				select {
				case w.events <- indexEvent{Name: event.Path}:
				case <-w.done:
					return
				}
			}
		}
	}
}
