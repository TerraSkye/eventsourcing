package memory_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	cqrs "github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/eventbus/memory"
)

// WidgetCreated is registered in the global event registry.
type WidgetCreated struct{ ID string }

func (e *WidgetCreated) AggregateID() string { return e.ID }
func (e *WidgetCreated) EventType() string   { return cqrs.TypeName(e) }

// WidgetRenamed is deliberately NOT registered. Registration is only needed for
// stores that rehydrate events by name; an in-memory bus never needs it.
type WidgetRenamed struct{ ID string }

func (e *WidgetRenamed) AggregateID() string { return e.ID }
func (e *WidgetRenamed) EventType() string   { return cqrs.TypeName(e) }

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

// TestStreamFilterIncludesUnregisteredEventTypes is a regression test for
// GitHub issue #55: StreamFilter derived its result from the global event
// registry via EventNamesFor instead of from the handlers it actually routes
// to, so any handled event type never passed to RegisterEvent was silently
// omitted. It asserts the documented contract of
// EventGroupProcessor.StreamFilter: "returns a sorted list of all event names
// handled by this group".
func TestStreamFilterIncludesUnregisteredEventTypes(t *testing.T) {
	cqrs.RegisterEvent(&WidgetCreated{})
	// WidgetRenamed intentionally not registered.

	group, _ := newGroup(t)

	want := []string{"WidgetCreated", "WidgetRenamed"}
	if got := group.StreamFilter(); !reflect.DeepEqual(got, want) {
		t.Errorf("StreamFilter() = %v, want %v", got, want)
	}
}

// UnrelatedEvent belongs to some other bounded context; the group has no handler for it.
type UnrelatedEvent struct{ ID string }

func (e *UnrelatedEvent) AggregateID() string { return e.ID }
func (e *UnrelatedEvent) EventType() string   { return cqrs.TypeName(e) }

// TestStreamFilterAllUnregisteredInvertsToCatchAll is a regression test for
// GitHub issue #55's inverted failure mode: when no handled type was
// registered, StreamFilter() returned an empty slice, which eventbus/memory
// reads as "no filter" — the subscriber was firehosed with every event on
// the bus instead of just the ones it has handlers for.
func TestStreamFilterAllUnregisteredInvertsToCatchAll(t *testing.T) {
	group := cqrs.NewEventGroupProcessor(
		cqrs.OnEvent(func(ctx context.Context, ev *WidgetRenamed) error { return nil }),
	)

	if f := group.StreamFilter(); len(f) != 1 {
		t.Fatalf("StreamFilter() = %v, want a single-element filter", f)
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
		t.Fatalf("subscriber filtered on StreamFilter() received unrelated event %T; "+
			"an empty filter means catch-all, so the group was subscribed to the whole bus", ev)
	case <-time.After(500 * time.Millisecond):
		// desired: never delivered
	}
}

// TestStreamFilterSubscriptionDeliversEveryHandledEvent is the end-to-end
// regression test for GitHub issue #55: wiring StreamFilter() into a
// subscription — the use the godoc advertises — must not silence handlers
// the group actually has just because their event type was never registered.
func TestStreamFilterSubscriptionDeliversEveryHandledEvent(t *testing.T) {
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

	deadline := time.After(2 * time.Second)
	for {
		if len(got.snapshot()) == 2 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("handlers called for %v, want both WidgetCreated and WidgetRenamed", got.snapshot())
		case <-time.After(10 * time.Millisecond):
		}
	}
}
