package memory_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	cqrs "github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/eventbus/memory"
)

type blockingEvent struct{}

func (blockingEvent) AggregateID() string { return "agg-1" }
func (blockingEvent) EventType() string   { return "blockingEvent" }

type blockingHandler struct {
	handling     chan struct{}
	handlingOnce sync.Once
	release      chan struct{}
}

func (h *blockingHandler) Handle(ctx context.Context, ev cqrs.Event) error {
	h.handlingOnce.Do(func() { close(h.handling) })
	<-h.release
	return nil
}

// TestDispatch_BlockingSendDoesNotHoldLock is a regression test for GitHub
// issue #31: Dispatch performed a blocking send to a subscriber's channel
// while still holding b.mu.RLock(). A slow or stuck handler left that send
// (and the RLock) pending forever, deadlocking Close (which needs
// b.mu.Lock()) and every other call that needs the lock — not just the one
// Dispatch call for the slow subscriber.
func TestDispatch_BlockingSendDoesNotHoldLock(t *testing.T) {
	bus := memory.NewEventBus(0) // unbuffered subscriber channel

	h := &blockingHandler{
		handling: make(chan struct{}),
		release:  make(chan struct{}),
	}

	if err := bus.Subscribe(context.Background(), "sub1", h); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// First event: runSubscriber's select receives it immediately (synchronous
	// handoff on the unbuffered channel) and calls Handle, which blocks on
	// h.release. This first Dispatch call itself returns fine.
	bus.Dispatch(&cqrs.Envelope{Event: blockingEvent{}})

	select {
	case <-h.handling:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never started")
	}

	// Second event: runSubscriber is busy inside Handle and not receiving
	// from s.events, and the channel is unbuffered, so this send blocks. It
	// must not do so while holding b.mu.
	go bus.Dispatch(&cqrs.Envelope{Event: blockingEvent{}})
	time.Sleep(200 * time.Millisecond)

	// A completely unrelated call that needs b.mu.Lock() must not be blocked
	// by the pending send above.
	subscribeDone := make(chan error, 1)
	go func() {
		subscribeDone <- bus.Subscribe(context.Background(), "sub2", cqrs.NewEventHandlerFunc(
			func(ctx context.Context, ev cqrs.Event) error { return nil },
		))
	}()

	select {
	case err := <-subscribeDone:
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Subscribe was blocked by the pending Dispatch send — b.mu is still held during the blocking send")
	}

	close(h.release)

	closeDone := make(chan struct{})
	go func() {
		_ = bus.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not complete after the handler was released")
	}
}

// WidgetCreated is registered in the global event registry.
type WidgetCreated struct{ ID string }

func (e *WidgetCreated) AggregateID() string { return e.ID }
func (e *WidgetCreated) EventType() string   { return "WidgetCreated" }

// WidgetRenamed is deliberately NOT registered. Registration is only needed for
// stores that rehydrate events by name; an in-memory bus never needs it.
type WidgetRenamed struct{ ID string }

func (e *WidgetRenamed) AggregateID() string { return e.ID }
func (e *WidgetRenamed) EventType() string   { return "WidgetRenamed" }

// calls is a race-free recorder of which handlers the group actually invoked.
type calls struct {
	mu   sync.Mutex
	seen []string
}

func (c *calls) add(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, name)
}

func (c *calls) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.seen)
}

func newGroup(t *testing.T) (*cqrs.EventGroupProcessor, *calls) {
	t.Helper()

	got := &calls{}
	group := cqrs.NewEventGroupProcessor(
		cqrs.OnEvent(func(ctx context.Context, ev *WidgetCreated) error {
			got.add("WidgetCreated")
			return nil
		}),
		cqrs.OnEvent(func(ctx context.Context, ev *WidgetRenamed) error {
			got.add("WidgetRenamed")
			return nil
		}),
	)
	return group, got
}

// TestStreamFilterOmitsUnregisteredEventTypes documents current behavior:
// StreamFilter derives its result from the global event registry via
// EventNamesFor, so any handled event type never passed to
// RegisterEvent/RegisterEventByType/RegisterEventByName is silently omitted
// rather than included under its own EventType().
func TestStreamFilterOmitsUnregisteredEventTypes(t *testing.T) {
	cqrs.RegisterEvent(&WidgetCreated{})
	// WidgetRenamed intentionally not registered.

	group, _ := newGroup(t)

	want := []string{"WidgetCreated"}
	if got := group.StreamFilter(); !reflect.DeepEqual(got, want) {
		t.Errorf("StreamFilter() = %v, want %v (WidgetRenamed is unregistered and silently omitted)", got, want)
	}
}

// UnrelatedEvent belongs to some other bounded context; the group has no handler for it.
type UnrelatedEvent struct{ ID string }

func (e *UnrelatedEvent) AggregateID() string { return e.ID }
func (e *UnrelatedEvent) EventType() string   { return "UnrelatedEvent" }

// TestStreamFilterAllUnregisteredInvertsToCatchAll documents a consequence
// of the current omit-unregistered behavior: when no handled type is
// registered, StreamFilter() returns an empty slice, which eventbus/memory
// reads as "no filter" — the subscriber ends up firehosed with every event
// on the bus instead of just the ones it has handlers for.
func TestStreamFilterAllUnregisteredInvertsToCatchAll(t *testing.T) {
	group := cqrs.NewEventGroupProcessor(
		cqrs.OnEvent(func(ctx context.Context, ev *WidgetRenamed) error { return nil }),
	)

	if f := group.StreamFilter(); len(f) != 0 {
		t.Fatalf("StreamFilter() = %v, want an empty filter (WidgetRenamed is unregistered and silently omitted)", f)
	}

	bus := memory.NewEventBus(8)
	defer bus.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	delivered := make(chan cqrs.Event, 4)
	probe := cqrs.NewEventHandlerFunc(func(ctx context.Context, ev cqrs.Event) error {
		delivered <- ev
		return group.Handle(ctx, ev)
	})

	if err := bus.Subscribe(ctx, "widgets", probe, memory.WithFilterEvents(group.StreamFilter())); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	bus.Dispatch(&cqrs.Envelope{StreamID: "x1", Event: &UnrelatedEvent{ID: "x1"}, OccurredAt: time.Now()})

	select {
	case ev := <-delivered:
		if _, ok := ev.(*UnrelatedEvent); !ok {
			t.Fatalf("delivered unexpected event type %T", ev)
		}
		// desired (current behavior): an empty StreamFilter() means
		// catch-all, so the group is subscribed to the whole bus, including
		// event types it has no handler for.
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("expected the unrelated event to be delivered: an empty StreamFilter() means catch-all")
	}
}

// TestStreamFilterSubscriptionDropsUnregisteredHandledEvent is the
// end-to-end counterpart of TestStreamFilterOmitsUnregisteredEventTypes:
// wiring StreamFilter() into a subscription silently drops events for any
// handled type that was never registered — WidgetRenamed is filtered out
// at the bus and never reaches the group, even though the group has a
// working handler for it.
func TestStreamFilterSubscriptionDropsUnregisteredHandledEvent(t *testing.T) {
	group, got := newGroup(t)

	bus := memory.NewEventBus(8)
	defer bus.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := bus.Subscribe(ctx, "widgets", group, memory.WithFilterEvents(group.StreamFilter())); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	bus.Dispatch(&cqrs.Envelope{StreamID: "w1", Event: &WidgetCreated{ID: "w1"}, OccurredAt: time.Now()})
	bus.Dispatch(&cqrs.Envelope{StreamID: "w1", Event: &WidgetRenamed{ID: "w1"}, OccurredAt: time.Now()})

	// Drain any handler errors so a skipped event surfaces instead of hiding.
	go func() {
		for err := range bus.Errors() {
			var skipped *cqrs.ErrSkippedEvent
			if !errors.As(err, &skipped) {
				t.Errorf("unexpected handler error: %v", err)
			}
		}
	}()

	// Wait for WidgetCreated to arrive, then give WidgetRenamed a fair
	// window to (not) show up too.
	deadline := time.After(2 * time.Second)
	for len(got.snapshot()) == 0 {
		select {
		case <-deadline:
			t.Fatalf("handlers called for %v, want [WidgetCreated]", got.snapshot())
		case <-time.After(10 * time.Millisecond):
		}
	}
	time.Sleep(300 * time.Millisecond)

	want := []string{"WidgetCreated"}
	if got := got.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("handlers called for %v, want %v (WidgetRenamed is unregistered, filtered out of the "+
			"subscription by StreamFilter(), and never delivered)", got, want)
	}
}

// TestUse_RacesWithSubscribe is a regression test for GitHub issue #33: Use
// appended to b.middlewares with no synchronization at all, and Subscribe
// read b.middlewares before acquiring b.mu, so calling Use concurrently with
// Subscribe was a data race under `go test -race`.
func TestUse_RacesWithSubscribe(t *testing.T) {
	bus := memory.NewEventBus(1)

	noopMiddleware := func(next cqrs.EventHandler) cqrs.EventHandler {
		return next
	}
	handler := cqrs.NewEventHandlerFunc(func(ctx context.Context, event cqrs.Event) error {
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

	bus := memory.NewEventBus(1)

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
