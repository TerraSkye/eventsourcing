package file

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
)

// TestFileEventBusCloseIdempotent is a regression test for GitHub issue #26:
// Close used to close(b.errs) unconditionally, so calling it a second time
// (e.g. once from an explicit shutdown path and again from a deferred
// cleanup, a common pattern) panicked with "close of closed channel"
// instead of being a no-op, unlike the sibling eventbus/memory implementation.
func TestFileEventBusCloseIdempotent(t *testing.T) {
	root := t.TempDir()
	bus, err := NewFileEventBus(root)
	if err != nil {
		t.Fatalf("NewFileEventBus: %v", err)
	}

	if err := bus.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	// A second Close() should be a no-op (or return an error), not panic.
	if err := bus.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestFileEventBusClose_LetsInFlightHandlerFinish covers the same "finish
// pending work, don't accept new work" shutdown concept CommandBus.Stop
// documents: Close must not cancel a handler call already in progress. It
// only stops the subscriber's loop from picking up anything further; a
// currently-running processFile/Handle call always uses context.Background()
// and completes normally even if Close runs concurrently.
func TestFileEventBusClose_LetsInFlightHandlerFinish(t *testing.T) {
	root := t.TempDir()
	bus, err := NewFileEventBus(root)
	if err != nil {
		t.Fatalf("NewFileEventBus: %v", err)
	}

	handlerStarted := make(chan struct{})
	release := make(chan struct{})
	ctxErrCh := make(chan error, 1)

	handler := eventsourcing.NewEventHandlerFunc(func(ctx context.Context, ev eventsourcing.Event) error {
		close(handlerStarted)
		<-release
		ctxErrCh <- ctx.Err()
		return nil
	})

	if err := bus.Subscribe(context.Background(), "sub1", handler); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Let the subscriber's fsnotify watcher come up before dispatching.
	time.Sleep(200 * time.Millisecond)

	if err := bus.Dispatch(&eventsourcing.Envelope{
		StreamID: "s1",
		Event:    orderPlaced{ID: "s1", Total: 1},
	}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	select {
	case <-handlerStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never started")
	}

	closeDone := make(chan struct{})
	go func() {
		bus.Close()
		close(closeDone)
	}()

	// Give Close time to cancel the subscriber's loop context while the
	// handler is still blocked inside Handle.
	time.Sleep(50 * time.Millisecond)
	close(release)

	select {
	case ctxErr := <-ctxErrCh:
		if ctxErr != nil {
			t.Fatalf("handler's context was canceled mid-flight: %v", ctxErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler never observed the release signal")
	}

	select {
	case <-closeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Close never returned")
	}
}

// countGoroutinesCreatedBy reports the number of goroutines in a full stack
// dump whose "created by" line contains substr.
func countGoroutinesCreatedBy(substr string) int {
	buf := make([]byte, 4<<20)
	n := runtime.Stack(buf, true)
	return strings.Count(string(buf[:n]), substr)
}

// TestSubscribe_CtxWatcherGoroutineLeaksAfterClose is a regression test for
// GitHub issue #27: Subscribe's ctx-watcher goroutine used to block on
// <-ctx.Done() alone, which never fires for a long-lived ctx such as
// context.Background() (used by every other test in this file, and by every
// example in the repo) — leaking one goroutine per Subscribe call for the
// rest of the process's life, unaffected by Close.
func TestSubscribe_CtxWatcherGoroutineLeaksAfterClose(t *testing.T) {
	const n = 20
	const createdBy = "created by github.com/terraskye/eventsourcing/eventbus/file.(*FileEventBus).Subscribe"

	root := t.TempDir()
	bus, err := NewFileEventBus(root)
	if err != nil {
		t.Fatalf("NewFileEventBus: %v", err)
	}

	before := countGoroutinesCreatedBy(createdBy)

	noop := eventsourcing.NewEventHandlerFunc(func(ctx context.Context, ev eventsourcing.Event) error {
		return nil
	})

	for i := 0; i < n; i++ {
		name := fmt.Sprintf("sub-%d", i)
		if err := bus.Subscribe(context.Background(), name, noop); err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
	}

	// Let the fsnotify watchers come up before closing.
	time.Sleep(200 * time.Millisecond)

	if err := bus.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Give any well-behaved goroutines a chance to exit.
	time.Sleep(300 * time.Millisecond)
	runtime.GC()

	after := countGoroutinesCreatedBy(createdBy)

	// Correct behaviour: once Close() has fully torn down the bus, no
	// ctx-watcher goroutines from Subscribe should remain running.
	leaked := after - before
	if leaked > 0 {
		t.Fatalf("expected 0 leaked ctx-watcher goroutines after Close, got %d (before=%d after=%d)", leaked, before, after)
	}
}

// TestFileEventBus_UseRacesWithSubscribe is a regression test for GitHub
// issue #28: Use appended to b.middlewares with no synchronization, and
// Subscribe read b.middlewares before acquiring b.mu, so calling Use
// concurrently with Subscribe was a data race under `go test -race`.
func TestFileEventBus_UseRacesWithSubscribe(t *testing.T) {
	root := t.TempDir()
	bus, err := NewFileEventBus(root)
	if err != nil {
		t.Fatalf("NewFileEventBus: %v", err)
	}
	defer bus.Close()

	noopMiddleware := func(next eventsourcing.EventHandler) eventsourcing.EventHandler {
		return next
	}
	handler := eventsourcing.NewEventHandlerFunc(func(ctx context.Context, event eventsourcing.Event) error {
		return nil
	})

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		bus.Use(noopMiddleware)
	}()

	go func() {
		defer wg.Done()
		_ = bus.Subscribe(context.Background(), "sub-1", handler)
	}()

	wg.Wait()
}

type orderPlaced struct {
	ID    string `json:"id"`
	Total int    `json:"total"`
}

func (e orderPlaced) AggregateID() string { return e.ID }
func (e orderPlaced) EventType() string   { return "order_placed" }

func init() {
	eventsourcing.RegisterEventByType(func() eventsourcing.Event { return &orderPlaced{} })
}

// TestFileEventBusDeliversDispatchedEvent is a regression test for GitHub
// issue #37: Envelope.Event is a bare interface with no custom unmarshaller,
// so json.Unmarshal into a fresh Envelope always failed, and processFile
// swallowed the error and returned — no subscriber ever received any event,
// and every dispatched file accumulated on disk forever.
func TestFileEventBusDeliversDispatchedEvent(t *testing.T) {
	root := t.TempDir()
	bus, err := NewFileEventBus(root)
	if err != nil {
		t.Fatalf("NewFileEventBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })

	got := make(chan eventsourcing.Event, 1)
	err = bus.Subscribe(
		context.Background(),
		"projector",
		eventsourcing.NewEventHandlerFunc(func(ctx context.Context, ev eventsourcing.Event) error {
			select {
			case got <- ev:
			default:
			}
			return nil
		}),
	)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Let the subscriber's fsnotify watcher come up.
	time.Sleep(200 * time.Millisecond)

	env := &eventsourcing.Envelope{
		EventID:       uuid.New(),
		StreamID:      "order-1",
		Event:         orderPlaced{ID: "order-1", Total: 42},
		Version:       1,
		GlobalVersion: 1,
		OccurredAt:    time.Now().UTC(),
	}

	if err := bus.Dispatch(env); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	select {
	case ev := <-got:
		if ev == nil {
			t.Fatalf("handler received a nil event")
		}
		if ev.EventType() != "order_placed" {
			t.Fatalf("handler received %T (%q), want order_placed", ev, ev.EventType())
		}
	case <-time.After(3 * time.Second):
		// Distinguish "Dispatch never wrote the file" from "the worker wrote it
		// but could never decode it". The latter was the actual bug.
		left, _ := os.ReadDir(filepath.Join(root, "projector"))
		names := make([]string, 0, len(left))
		for _, e := range left {
			names = append(names, e.Name())
		}
		t.Fatalf("handler never received the dispatched event; undelivered files still on disk: %v", names)
	}
}

// TestFileEventBusCrashRecoverySkipsTmpFiles documents a bug: see
// .bug/eventbus-file-crash-recovery-sweep-processes-tmp-files.md.
//
// Dispatch writes each event atomically via a write-then-rename dance
// (path+".tmp", then os.Rename to path), and runSubscriber's fsnotify loop
// explicitly skips any ".tmp"-suffixed path so it never treats an in-flight
// write as a deliverable event. But runSubscriber's crash-recovery sweep
// (the os.ReadDir(dir) loop that replays files already present when
// Subscribe is called) applies no such filter: it hands every non-directory
// entry — including a stray ".tmp" file left by a Dispatch that wrote its
// data but had not yet renamed it (a live race, or a crash exactly between
// the two syscalls) — straight to processFile, which decodes it, delivers
// it to the handler, and deletes it from its ".tmp" path. That races the
// in-flight Dispatch's own os.Rename(tmp, path) call, which then fails
// (silently — Dispatch discards the error) because its source file is
// already gone, so the canonical ".json" file the naming scheme expects
// never comes to exist.
func TestFileEventBusCrashRecoverySkipsTmpFiles(t *testing.T) {

	root := t.TempDir()
	bus, err := NewFileEventBus(root)
	if err != nil {
		t.Fatalf("NewFileEventBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })

	subDir := filepath.Join(root, "sub1")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// Simulate the exact state Dispatch's write-then-rename dance passes
	// through: a fully-written ".tmp" file that has not (yet) been renamed
	// to its final ".json" name — e.g. because Dispatch is paused right
	// between the WriteFile and Rename calls, or because the process
	// crashed in that window on a prior run.
	eventData, err := json.Marshal(orderPlaced{ID: "order-1", Total: 42})
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	stored, err := json.Marshal(storedEvent{
		EventID:   uuid.New(),
		StreamID:  "order-1",
		EventType: "order_placed",
		Data:      eventData,
		Version:   1,
	})
	if err != nil {
		t.Fatalf("marshal storedEvent: %v", err)
	}
	tmpPath := filepath.Join(subDir, "00000000000000000001.json.tmp")
	if err := os.WriteFile(tmpPath, stored, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	delivered := make(chan struct{}, 1)
	handler := eventsourcing.NewEventHandlerFunc(func(ctx context.Context, ev eventsourcing.Event) error {
		select {
		case delivered <- struct{}{}:
		default:
		}
		return nil
	})

	if err := bus.Subscribe(context.Background(), "sub1", handler); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	select {
	case <-delivered:
		t.Fatalf("crash-recovery sweep delivered a %q file, matching the live fsnotify path's "+
			"explicit skip of such files would have left it untouched", filepath.Base(tmpPath))
	case <-time.After(500 * time.Millisecond):
	}

	if _, err := os.Stat(tmpPath); err != nil {
		t.Fatalf("stat %s: %v (crash-recovery sweep deleted a still-in-flight .tmp file)", tmpPath, err)
	}
}

// TestDispatch_ConcurrentCallsLoseEventsToFilenameCollision documents a bug:
// see .bug/eventbus-file-dispatch-concurrent-filename-collision-loses-events.md.
//
// Dispatch names each subscriber's file solely from the wall-clock time of
// the call — fmt.Sprintf("%020d.json", time.Now().UnixNano()) — with no
// per-event uniqueness (no EventID, no counter). time.Now().UnixNano() is not
// guaranteed unique between calls; two goroutines calling Dispatch at
// genuinely the same nanosecond derive the identical filename for the same
// subscriber directory. os.Rename(tmp, path) then silently replaces
// whichever file got there first — even one still awaiting pickup by the
// subscriber's fsnotify watcher — so the earlier event is never delivered
// and leaves no trace on disk.
func TestDispatch_ConcurrentCallsLoseEventsToFilenameCollision(t *testing.T) {

	root := t.TempDir()
	bus, err := NewFileEventBus(root)
	if err != nil {
		t.Fatalf("NewFileEventBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })

	var mu sync.Mutex
	received := make(map[string]bool)

	err = bus.Subscribe(
		context.Background(),
		"sub",
		eventsourcing.NewEventHandlerFunc(func(ctx context.Context, ev eventsourcing.Event) error {
			if op, ok := ev.(*orderPlaced); ok {
				mu.Lock()
				received[op.ID] = true
				mu.Unlock()
			}
			return nil
		}),
	)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Let the subscriber's fsnotify watcher come up.
	time.Sleep(200 * time.Millisecond)

	const goroutines = 16
	const perGoroutine = 500
	total := goroutines * perGoroutine

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				id := fmt.Sprintf("order-%d-%d", g, i)
				env := &eventsourcing.Envelope{
					EventID:    uuid.New(),
					StreamID:   id,
					Event:      orderPlaced{ID: id, Total: i},
					OccurredAt: time.Now().UTC(),
				}
				if err := bus.Dispatch(env); err != nil {
					t.Errorf("Dispatch: %v", err)
				}
			}
		}(g)
	}
	wg.Wait()

	// Give the subscriber time to process every file that actually made it
	// to disk before checking how many distinct events it saw.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(received)
		mu.Unlock()
		if n >= total {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != total {
		t.Fatalf("dispatched %d events concurrently, only %d were delivered — %d were silently "+
			"lost to Dispatch's time.Now().UnixNano() filename collisions", total, len(received), total-len(received))
	}
}
