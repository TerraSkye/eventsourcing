package eventsourcing

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// EventHandler processes events delivered by an [EventBus] or
// [EventGroupProcessor].
type EventHandler interface {
	// Handle processes event within ctx.
	Handle(ctx context.Context, event Event) error
}

// NewEventHandlerFunc returns fn as an [EventHandler], for quickly wrapping
// a function without defining a separate type. fn is called for every event
// it is invoked with, unfiltered by type, so this is the constructor to reach
// for when fn switches on the event type itself:
//
//	switch ev := event.(type) {
//	case *CartCreated:
//	case *ItemAdded:
//	}
//
// The returned handler cannot be registered with an [EventGroupProcessor] —
// that requires each handler to report the single event type name it handles,
// which a handler for every event type has no one value for. Use [OnEvent]
// for a handler usable there; it takes one concrete event type and will not
// compile against [Event] itself, since a handler for the interface could
// never be routed to.
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

// typedEventHandler is an [EventHandler] for one specific event type, handled
// as *T. Carrying the value type T alongside the pointer type PT is what lets
// it name and instantiate the type without reflection or a nil receiver.
type typedEventHandler[T any, PT eventPtr[T]] func(ctx context.Context, ev PT) error

// EventName returns the routing key [EventGroupProcessor] files this handler
// under: T's package-qualified type name, which is the key the event registry
// uses too, so a handler resolves to the names its type is registered under.
//
// T is the value type, so there is no pointer to strip, and [reflect.TypeFor]
// reads it without a value to box or a zero to construct.
func (h typedEventHandler[T, PT]) EventName() string {
	return reflect.TypeFor[T]().String()
}

// EventInstance returns a newly allocated instance of the event type.
//
// It is PT(new(T)) rather than the zero PT, which would be a nil pointer:
// anything that reaches a method through the returned value — a value-receiver
// [Event.EventType], say — would dereference that nil and panic before the
// method body ran.
func (h typedEventHandler[T, PT]) EventInstance() Event {
	return PT(new(T))
}

// Handle calls h with event if it has type PT, or returns [SkippedEventError]
// otherwise.
func (h typedEventHandler[T, PT]) Handle(ctx context.Context, event Event) error {
	ev, ok := event.(PT)
	if !ok {
		return &SkippedEventError{Event: event}
	}
	return h(ctx, ev)
}

// OnEvent returns fn as an [EventHandler] that only processes events of type
// *T, returning [SkippedEventError] for any other type. It is meant to be
// registered with an [EventGroupProcessor], which uses the type's name to
// route only matching events to it.
//
// fn must take a pointer, which is the form events arrive in: [RegisterEvent]
// registers a type by minting new(T), so [NewEventByName] — and therefore
// every [EventStore] and [EventBus] that rehydrates an event by name — hands
// out *T. A handler written over the value type would be routed under a
// different name than the events it is meant to receive, so it would compile,
// register, appear in [EventGroupProcessor.StreamFilter], and silently never
// be called.
//
// Example Usage:
//
//	handler := OnEvent(func(ctx context.Context, ev *OrderCreated) error {
//	    fmt.Println("Order created:", ev.AggregateID())
//	    return nil
//	})
//	group := NewEventGroupProcessor(handler)
//	group.Handle(ctx, &OrderCreated{ID: "123"})
func OnEvent[T any, PT eventPtr[T]](fn func(ctx context.Context, ev PT) error) EventHandler {
	return typedEventHandler[T, PT](fn)
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

		// Normalised, so a hand-rolled handler reporting the pointer form —
		// the natural choice, since that is what Handle receives — is filed
		// under the same key as the registry and as the handlers OnEvent
		// builds. Keying it verbatim put it in a second namespace, where it
		// routed correctly but could never be resolved back to a registered
		// name.
		name := strings.TrimPrefix(u.EventName(), "*")
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
// returns [SkippedEventError] if none is registered.
func (p *EventGroupProcessor) Handle(ctx context.Context, ev Event) error {
	h, ok := p.handlers[eventTypeKey(ev)]

	if !ok {
		return &SkippedEventError{Event: ev}
	}
	return h.Handle(ctx, ev)
}

// StreamFilter returns the sorted names of every registered event type this group has
// a handler for — useful, for example, as the filter passed to
// [EventBus.Subscribe] via a filtering [SubscriberOption]. For a handled
// type registered in the global event registry (see [EventNamesFor]) it
// includes every name that type is registered under, since one concrete
// event struct can be registered under several names (for example after a
// rename, via [RegisterEventByName]) and a subscriber needs to match all of
// them. A handled type that isn't registered contributes no name and is
// silently omitted from the result, rather than falling back to that type's
// own [Event.EventType].
//
// A handler that does not expose an EventInstance is resolved through its
// routing key instead, which is the same type key the registry files names
// under. [NewEventGroupProcessor] requires only EventName, so a hand-rolled
// handler that never implements EventInstance is a supported shape and is
// not dropped for it.
func (p *EventGroupProcessor) StreamFilter() []string {
	out := make([]string, 0, len(p.handlers))
	for key, h := range p.handlers {
		if ei, ok := h.(interface{ EventInstance() Event }); ok {
			out = append(out, EventNamesFor(ei.EventInstance())...)
			continue
		}
		out = append(out, eventNamesForKey(key)...)
	}
	sort.Strings(out) // deterministic order
	return out
}
