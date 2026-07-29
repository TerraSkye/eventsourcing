package eventsourcing

import (
	"context"
	"fmt"
	"sort"
)

// EventHandler processes events delivered by an [EventBus] or
// [EventGroupProcessor].
type EventHandler interface {
	// Handle processes event within ctx.
	Handle(ctx context.Context, event Event) error
}

// NewEventHandlerFunc returns fn as an [EventHandler], for quickly wrapping
// a function without defining a separate type. fn is called for every event
// it is invoked with, unfiltered by type. The returned handler cannot be
// registered with an [EventGroupProcessor] — that requires each handler to
// report the single event type name it handles, which a handler for every
// event type has no one value for — so use [OnEvent] instead if you want a
// handler usable there, or if you only want to handle one concrete event
// type.
//
// Example Usage:
//
//	handler := NewEventHandlerFunc(func(ctx context.Context, ev Event) error {
//	    fmt.Println("Received event:", TypeName(ev))
//	    return nil
//	})
//	err := bus.Subscribe(ctx, "logger", handler)
func NewEventHandlerFunc(fn func(ctx context.Context, event Event) error) EventHandler {
	return eventHandlerFunc(fn)
}

// eventHandlerFunc is a function type that implements EventHandler.
type eventHandlerFunc func(ctx context.Context, event Event) error

func (h eventHandlerFunc) Handle(ctx context.Context, event Event) error {
	return h(ctx, event)
}

// typedEventHandler is an [EventHandler] for one specific Event type T,
// keyed for [EventGroupProcessor] routing by T's package-qualified type
// name (the same %T-derived form [EventNamesFor] keys its internal
// type-to-name lookup by).
type typedEventHandler[T Event] func(ctx context.Context, ev T) error

// EventName returns T's package-qualified type name, used as the routing
// key by [EventGroupProcessor].
func (h typedEventHandler[T]) EventName() string {
	var zero T
	return fmt.Sprintf("%T", zero)
}

// EventInstance returns a zero-value instance of the event type T.
func (h typedEventHandler[T]) EventInstance() Event {
	var zero T
	return zero
}

// Handle calls h with event if it has type T, or returns [ErrSkippedEvent]
// otherwise.
func (h typedEventHandler[T]) Handle(ctx context.Context, event Event) error {
	ev, ok := event.(T)
	if !ok {
		return &ErrSkippedEvent{Event: event}
	}
	return h(ctx, ev)
}

// OnEvent returns fn as an [EventHandler] that only processes events of
// type T, returning [ErrSkippedEvent] for any other type. It is meant to be
// registered with an [EventGroupProcessor], which uses T's type name to
// route only matching events to it.
//
// Example Usage:
//
//	handler := OnEvent(func(ctx context.Context, ev OrderCreated) error {
//	    fmt.Println("Order created:", ev.AggregateID())
//	    return nil
//	})
//	group := NewEventGroupProcessor(handler)
//	group.Handle(ctx, OrderCreated{ID: "123"})
func OnEvent[T Event](fn func(ctx context.Context, ev T) error) EventHandler {
	return typedEventHandler[T](fn)
}

// EventGroupProcessor routes each incoming event to the [EventHandler]
// registered for its concrete type, typically one built with [OnEvent].
type EventGroupProcessor struct {
	handlers map[string]EventHandler // key = EventName()
}

// NewEventGroupProcessor builds an [EventGroupProcessor] from handlers,
// which must each implement an internal EventName() string method — as the
// handlers returned by [OnEvent] do — used as the routing key. It panics if
// a handler doesn't implement that method, or if two handlers report the
// same EventName().
//
// Example Usage:
//
//	p := &Projector{}
//	group := NewEventGroupProcessor(
//	    OnEvent(p.OnCartCreated),
//	    OnEvent(p.OnItemAdded),
//	)
//	group.Handle(ctx, CartCreated{ID: "t1"})
//	group.Handle(ctx, ItemAdded{ID: "c1"})
func NewEventGroupProcessor(handlers ...EventHandler) *EventGroupProcessor {
	m := make(map[string]EventHandler, len(handlers))
	for _, h := range handlers {

		u, ok := h.(interface{ EventName() string })
		if !ok {
			panic(fmt.Errorf("handler %T does not have a function `EventName()`", h))
		}

		name := u.EventName()
		if _, exists := m[name]; exists {
			panic(fmt.Errorf("duplicate handler for event %s: %w", name, ErrDuplicateHandler))
		}
		m[name] = h
	}

	return &EventGroupProcessor{
		handlers: m,
	}
}

// Handle routes ev to the handler registered for its concrete type, or
// returns [ErrSkippedEvent] if none is registered.
func (p *EventGroupProcessor) Handle(ctx context.Context, ev Event) error {
	name := fmt.Sprintf("%T", ev)
	h, ok := p.handlers[name]

	if !ok {
		return &ErrSkippedEvent{Event: ev}
	}
	return h.Handle(ctx, ev)
}

// StreamFilter returns the sorted names of every event type this group has
// a handler for — useful, for example, as the filter passed to
// [EventBus.Subscribe] via a filtering [SubscriberOption]. For a handled
// type registered in the global event registry (see [EventNamesFor]) it
// includes every name that type is registered under, since one concrete
// event struct can be registered under several names (for example after a
// rename, via [RegisterEventByName]) and a subscriber needs to match all of
// them. For a handled type that isn't registered at all — registration is
// only needed by stores that rehydrate events by name, and has no bearing
// on what a group actually handles — it falls back to that type's own
// [Event.EventType], so an unregistered handled type is never silently
// dropped from the filter.
func (p *EventGroupProcessor) StreamFilter() []string {
	out := make([]string, 0, len(p.handlers))
	for _, h := range p.handlers {
		if ei, ok := h.(interface{ EventInstance() Event }); ok {
			instance := ei.EventInstance()
			if names := EventNamesFor(instance); len(names) > 0 {
				out = append(out, names...)
			} else {
				out = append(out, instance.EventType())
			}
		}
	}
	sort.Strings(out) // deterministic order
	return out
}
