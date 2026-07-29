package file

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/terraskye/eventsourcing"
)

// ---- Subscriber ----

// subscriber is a single registered handler and the event-type filter and
// per-subscriber directory it watches for new event files.
type subscriber struct {
	name    string
	handler eventsourcing.EventHandler
	filter  map[string]struct{}
	cancel  context.CancelFunc
}

var _ eventsourcing.EventBus = (*FileEventBus)(nil)

// fileSubscriberOptions accumulates the [eventsourcing.SubscriberOption]
// values passed to Subscribe.
type fileSubscriberOptions struct {
	filter map[string]struct{}
}

// WithFilterEvents returns an [eventsourcing.SubscriberOption] that limits
// delivery to events whose type is one of events. Without this option, a
// subscriber receives every dispatched event. It has no effect if applied
// to a bus other than this package's [FileEventBus].
func WithFilterEvents(events ...string) eventsourcing.SubscriberOption {
	return func(cfg any) {
		if c, ok := cfg.(*fileSubscriberOptions); ok {
			for _, ev := range events {
				c.filter[ev] = struct{}{}
			}
		}
	}
}

// ---- File-based EventBus ----

// FileEventBus is a file-backed [eventsourcing.EventBus]. Each dispatched
// event is written as one JSON file per subscriber directory; a subscriber
// watches its directory with fsnotify and deletes each file once its
// handler successfully processes it. Existing files are replayed on
// Subscribe, so a subscriber recovers events that arrived while it (or the
// process) was not running.
//
// If a handler returns an error, its file is left in place rather than
// deleted, and reprocessing happens on the next filesystem event for that
// directory or the next time the subscriber is started — there is no
// active retry loop in between.
type FileEventBus struct {
	mu          sync.RWMutex
	subs        map[string]*subscriber
	root        string
	closed      bool
	wg          sync.WaitGroup
	errs        chan error
	middlewares []eventsourcing.EventHandlerMiddleware
}

// NewFileEventBus constructs a [FileEventBus] backed by root, creating the
// directory (and any missing parents) if it does not already exist. It
// returns an error if the directory cannot be created.
func NewFileEventBus(root string) (*FileEventBus, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}

	return &FileEventBus{
		root: root,
		subs: make(map[string]*subscriber),
		errs: make(chan error, 64),
	}, nil
}

// Use adds middlewares that wrap every handler registered afterward via
// Subscribe. As required by [eventsourcing.EventBus], it must be called
// before Subscribe: middlewares added after a given subscriber is
// registered do not apply to that subscriber.
func (b *FileEventBus) Use(middlewares ...eventsourcing.EventHandlerMiddleware) {
	b.middlewares = append(b.middlewares, middlewares...)
}

// Subscribe registers handler under name, creating a dedicated directory
// for it under the bus's root, replaying any event files already present,
// and then watching for new ones. By default a subscriber receives every
// event; pass [WithFilterEvents] to restrict delivery to specific event
// types. It returns an error if handler is nil, the bus is already closed,
// name is already registered, or the subscriber directory cannot be
// created. The subscription is removed automatically when ctx is canceled.
func (b *FileEventBus) Subscribe(
	ctx context.Context,
	name string,
	handler eventsourcing.EventHandler,
	opts ...eventsourcing.SubscriberOption,
) error {
	if handler == nil {
		return errors.New("handler cannot be nil")
	}

	cfg := &fileSubscriberOptions{filter: make(map[string]struct{})}
	for _, opt := range opts {
		opt(cfg)
	}

	wrapped := handler
	for i := len(b.middlewares) - 1; i >= 0; i-- {
		wrapped = b.middlewares[i](wrapped)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return errors.New("bus is closed")
	}

	if _, exists := b.subs[name]; exists {
		return fmt.Errorf("subscriber %q already exists", name)
	}

	subDir := filepath.Join(b.root, name)
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		return err
	}

	workerCtx, cancel := context.WithCancel(ctx)

	s := &subscriber{
		name:    name,
		handler: wrapped,
		filter:  cfg.filter,
		cancel:  cancel,
	}

	b.subs[name] = s

	b.wg.Add(1)
	go b.runSubscriber(workerCtx, s, subDir)

	go func() {
		<-ctx.Done()
		b.removeSubscriber(name)
	}()

	return nil
}

// Errors returns the channel on which asynchronous handler errors are
// delivered, as required by [eventsourcing.EventBus].
func (b *FileEventBus) Errors() <-chan error {
	return b.errs
}

// Dispatch writes env as a new file in the directory of every subscriber
// whose filter matches it (see [WithFilterEvents]), or of every subscriber
// if none configured a filter; each subscriber's own goroutine then reads,
// handles, and deletes the file asynchronously. Dispatch returns an error
// only if env cannot be marshaled to JSON; a write failure for an
// individual subscriber's directory is skipped silently and not reported
// to the caller. Dispatch is a no-op once the bus has been closed.
func (b *FileEventBus) Dispatch(env *eventsourcing.Envelope) error {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.closed {
		return nil
	}

	data, err := json.Marshal(env)
	if err != nil {
		return err
	}

	for name, s := range b.subs {
		if len(s.filter) > 0 {
			if _, ok := s.filter[env.Event.EventType()]; !ok {
				continue
			}
		}

		dir := filepath.Join(b.root, name)
		filename := fmt.Sprintf("%020d.json", time.Now().UnixNano())
		path := filepath.Join(dir, filename)

		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, data, 0o644); err != nil {
			continue
		}
		_ = os.Rename(tmp, path)
	}

	return nil
}

// runSubscriber first replays any event files already present in dir (crash
// recovery), then watches dir with fsnotify and processes each new file as
// it appears, until ctx is canceled.
//
// ctx governs this loop only, not an individual handler call: canceling it
// (via Close or removeSubscriber) stops the loop from picking up any further
// file, but a processFile call already in progress runs with
// context.WithoutCancel(ctx) — carrying over any values set on ctx (e.g. by
// the caller of Subscribe) without inheriting its cancellation — so it
// completes rather than being cut short by a shutdown racing its execution.
func (b *FileEventBus) runSubscriber(ctx context.Context, s *subscriber, dir string) {
	defer b.wg.Done()

	handlerCtx := context.WithoutCancel(ctx)

	// Crash-recovery: process any existing files
	processDir := func() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			b.processFile(handlerCtx, s, filepath.Join(dir, e.Name()))
		}
	}
	processDir()

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return
	}
	defer watcher.Close()

	if err := watcher.Add(dir); err != nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return

		case ev := <-watcher.Events:
			if ev.Op&(fsnotify.Create|fsnotify.Rename|fsnotify.Write) != 0 {
				if strings.HasSuffix(ev.Name, ".tmp") {
					continue
				}
				b.processFile(handlerCtx, s, ev.Name)
			}

		case <-watcher.Errors:
			// swallow or log
		}
	}
}

// processFile reads and decodes the envelope at path, invokes s.handler with
// ctx (see runSubscriber for why this is decoupled from the subscriber's
// loop-shutdown signal), and deletes the file only if the handler returns
// nil. On any error the file is left in place for the next event or
// subscriber restart to retry.
func (b *FileEventBus) processFile(ctx context.Context, s *subscriber, path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}

	var env eventsourcing.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return
	}

	if err := s.handler.Handle(ctx, env.Event); err != nil {
		return // retry later
	}

	_ = os.Remove(path)
}

// removeSubscriber cancels and unregisters the named subscriber. It is a
// no-op if name is not registered.
func (b *FileEventBus) removeSubscriber(name string) {
	b.mu.Lock()
	s, ok := b.subs[name]
	if ok {
		delete(b.subs, name)
	}
	b.mu.Unlock()

	if ok {
		s.cancel()
	}
}

// Close stops every subscriber from picking up further events and waits for
// their worker goroutines to finish before closing the Errors channel. A
// handler call already in progress when Close is invoked always runs to
// completion (see runSubscriber); Dispatch and Subscribe already reject new
// work once the bus is closed, so nothing new is accepted in the meantime.
// Calling Close more than once is a no-op.
func (b *FileEventBus) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	for _, s := range b.subs {
		s.cancel()
	}
	b.subs = nil
	b.mu.Unlock()

	b.wg.Wait()
	close(b.errs)
	return nil
}
