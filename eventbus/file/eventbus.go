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
type subscriber struct {
	name    string
	handler eventsourcing.EventHandler
	filter  map[string]struct{}
	cancel  context.CancelFunc
}

// ---- File-based EventBus ----
type FileEventBus struct {
	mu     sync.RWMutex
	subs   map[string]*subscriber
	root   string
	closed bool
	wg     sync.WaitGroup
}

// NewFileEventBus constructs the bus in root dir
func NewFileEventBus(root string) (*FileEventBus, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}

	return &FileEventBus{
		root: root,
		subs: make(map[string]*subscriber),
	}, nil
}

// Subscribe registers a subscriber with optional event filters
func (b *FileEventBus) Subscribe(
	ctx context.Context,
	name string,
	handler eventsourcing.EventHandler,
	filteredEvents ...string,
) error {
	if handler == nil {
		return errors.New("handler cannot be nil")
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return errors.New("bus is closed")
	}

	if _, exists := b.subs[name]; exists {
		return fmt.Errorf("subscriber %q already exists", name)
	}

	filter := make(map[string]struct{})
	for _, ev := range filteredEvents {
		filter[ev] = struct{}{}
	}

	subDir := filepath.Join(b.root, name)
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		return err
	}

	workerCtx, cancel := context.WithCancel(context.Background())

	s := &subscriber{
		name:    name,
		handler: handler,
		filter:  filter,
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

// Dispatch writes the event to all matching subscriber directories
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

// runSubscriber watches the subscriber directory for new events
func (b *FileEventBus) runSubscriber(ctx context.Context, s *subscriber, dir string) {
	defer b.wg.Done()

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
			b.processFile(ctx, s, filepath.Join(dir, e.Name()))
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
				b.processFile(ctx, s, ev.Name)
			}

		case <-watcher.Errors:
			// swallow or log
		}
	}
}

// processFile reads and handles a single event file, then deletes on success
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

// removeSubscriber cancels and removes a subscriber
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

// Close shuts down the bus and waits for workers
func (b *FileEventBus) Close() error {
	b.mu.Lock()
	b.closed = true
	for _, s := range b.subs {
		s.cancel()
	}
	b.subs = nil
	b.mu.Unlock()

	b.wg.Wait()
	return nil
}
