package memory

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	cqrs "github.com/terraskye/eventsourcing"
)

func countGoroutinesCreatedBy(substr string) int {
	buf := make([]byte, 4<<20)
	n := runtime.Stack(buf, true)
	return strings.Count(string(buf[:n]), substr)
}

// TestSubscribe_CtxWatcherGoroutineLeaksAfterClose is a regression test for
// GitHub issue #32: the goroutine that auto-removes a subscriber blocked on
// <-ctx.Done() alone, which never fires for a long-lived ctx such as
// context.Background() (used by every other test/example in this repo) —
// leaking one goroutine per Subscribe call for the rest of the process's
// life, unaffected by Close.
func TestSubscribe_CtxWatcherGoroutineLeaksAfterClose(t *testing.T) {
	const n = 50
	const createdBy = "created by github.com/terraskye/eventsourcing/eventbus/memory.(*EventBus).Subscribe"

	bus := NewEventBus(1)

	before := countGoroutinesCreatedBy(createdBy)

	noop := cqrs.NewEventHandlerFunc(func(ctx context.Context, ev cqrs.Event) error { return nil })

	for i := 0; i < n; i++ {
		name := fmt.Sprintf("sub-%d", i)
		if err := bus.Subscribe(context.Background(), name, noop); err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
	}

	if err := bus.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Give any well-behaved goroutines a chance to exit.
	time.Sleep(200 * time.Millisecond)
	runtime.GC()

	after := countGoroutinesCreatedBy(createdBy)

	leaked := after - before
	if leaked > 0 {
		t.Fatalf("expected 0 leaked ctx-watcher goroutines after Close, got %d (before=%d after=%d)", leaked, before, after)
	}
}
