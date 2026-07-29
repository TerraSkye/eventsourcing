package file

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

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
