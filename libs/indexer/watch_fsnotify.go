//go:build !darwin

package indexer

import (
	"sync"

	"github.com/fsnotify/fsnotify"
)

type fsnotifyIndexWatcher struct {
	raw       *fsnotify.Watcher
	events    chan indexEvent
	errs      chan error
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
}

func newIndexWatcher(string) (indexWatcher, error) {
	raw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	w := &fsnotifyIndexWatcher{
		raw: raw, events: make(chan indexEvent), errs: make(chan error), done: make(chan struct{}),
	}
	go w.run()
	return w, nil
}

func (w *fsnotifyIndexWatcher) Add(name string) error     { return w.raw.Add(name) }
func (w *fsnotifyIndexWatcher) Events() <-chan indexEvent { return w.events }
func (w *fsnotifyIndexWatcher) Errors() <-chan error      { return w.errs }

func (w *fsnotifyIndexWatcher) Close() error {
	w.closeOnce.Do(func() {
		close(w.done)
		w.closeErr = w.raw.Close()
	})
	return w.closeErr
}

func (w *fsnotifyIndexWatcher) run() {
	defer close(w.events)
	defer close(w.errs)
	events, errs := w.raw.Events, w.raw.Errors
	for events != nil || errs != nil {
		select {
		case <-w.done:
			return
		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			select {
			case w.events <- indexEvent{Name: event.Name}:
			case <-w.done:
				return
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			select {
			case w.errs <- err:
			case <-w.done:
				return
			}
		}
	}
}
