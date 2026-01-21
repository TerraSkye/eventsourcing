package eventsourcing

import (
	"context"
	"fmt"
	"sort"
)

// EventHandler represents a generic event handler that can handle an Event.
type EventHandler interface {
	// Handle processes the given Event within the provided context.
	Handle(ctx context.Context, event Event) error
}

// NewEventHandlerFunc creates an EventHandler from a plain function.
//
// This is a helper for quickly creating an EventHandler without defining
// a separate struct. It wraps the provided function and implements
// the EventHandler interface, allowing it to be used wherever an
// EventHandler is required, such as in an EventHandlerGroup.
//
// Parameters:
//   - fn: A function with signature func(ctx context.Context, ev Event) error.
//     This function will be called whenever the EventHandler receives an event.
//
// Returns:
//   - EventHandler: an implementation of the EventHandler interface
//     that delegates event handling to the provided function.
//
// Behavior Details:
//   - The provided function is called for every event passed to the EventHandler.
//   - There is no type-checking or filtering: the handler will receive all events
//     that it is invoked with. If you need type safety, use OnEvent[T] instead.
//   - Any error returned by the function is propagated directly to the caller.
//
// Example Usage:
//
//	handler := NewEventHandlerFunc(func(ctx context.Context, ev Event) error {
//	    fmt.Println("Received event:", TypeName(ev))
//	    return nil
//	})
//
//	// Can be used in an EventHandlerGroup
//	group := NewEventGroupProcessor(handler)
//	group.Handle(ctx, MyEvent{ID: "123"})
func NewEventHandlerFunc(fn func(ctx context.Context, event Event) error) EventHandler {
	return eventHandlerFunc(fn)
}

// eventHandlerFunc is a function type that implements EventHandler.
type eventHandlerFunc func(ctx context.Context, event Event) error

func (h eventHandlerFunc) Handle(ctx context.Context, event Event) error {
	return h(ctx, event)
}

// typedEventHandler is a strongly typed event handler for a specific Event type T.
type typedEventHandler[T Event] func(ctx context.Context, ev T) error

// EventName returns the name of the event type T.
// It is used internally by eventGroupProcessor for routing.
func (h typedEventHandler[T]) EventName() string {
	var zero T
	return TypeName(zero)
}

// Handle processes the event if it matches the type T.
// Returns ErrSkippedEvent if the event is of the wrong type.
func (h typedEventHandler[T]) Handle(ctx context.Context, event Event) error {
	ev, ok := event.(T)
	if !ok {
		// Return sentinel error instead of voiding it
		return &ErrSkippedEvent{Event: event}
	}
	return h(ctx, ev)
}

// OnEvent creates a strongly-typed EventHandler for a specific event type.
//
// It provides a reusable pattern for handling events in a type-safe manner
// by performing the following steps:
//  1. Wraps a user-provided function `fn(ctx, ev T)` into a typed handler.
//  2. Associates the handler with the type name of T for internal routing.
//  3. Returns an EventHandler that can be registered in an eventGroupProcessor.
//
// Parameters:
//   - fn: A function with signature func(ctx context.Context, ev T) error,
//     where T implements the Event interface.
//
// Returns:
//   - EventHandler: a strongly-typed handler for events of type T.
//
// Behavior Details:
//   - When called via eventGroupProcessor.Handle, the handler will only receive
//     events of type T. If a different event type is passed, it returns
//     ErrSkippedEvent.
//   - EventName() internally derives the type name of T using TypeName[T].
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

// EventGroupProcessor is a collection of typed event handlers.
// It routes incoming events to the correct handler based on event type.
type EventGroupProcessor struct {
	handlers map[string]EventHandler // key = EventName()
}

// NewEventGroupProcessor creates a group of typed event handlers.
//
// It provides a reusable pattern for routing events to the correct handler
// by performing the following steps:
//  1. Accepts a list of typed EventHandler instances (created via OnEvent).
//  2. Validates that all handlers implement EventName().
//  3. Builds an internal map from EventName() to EventHandler for fast routing.
//  4. Panics if duplicate handlers are provided for the same event type.
//
// Parameters:
//   - handlers: A variadic list of typed EventHandler instances.
//
// Returns:
//   - *eventGroupProcessor: a processor that routes events to the appropriate handler.
//
// Behavior Details:
//   - Events are dispatched based on the EventName() returned by the handler.
//   - If no handler exists for an event, Handle returns ErrSkippedEvent.
//   - StreamFilter() returns the sorted list of event names handled by the group.
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

// Handle routes the given event to the correct typed handler.
// Returns ErrSkippedEvent if no handler exists for the event type.
func (p *EventGroupProcessor) Handle(ctx context.Context, ev Event) error {
	name := ev.EventType()
	h, ok := p.handlers[name]

	if !ok {
		return &ErrSkippedEvent{Event: ev}
	}
	return h.Handle(ctx, ev)
}

// StreamFilter returns a sorted list of all event names handled by this group.
// Useful for subscribing to streams or listing registered handlers.
func (p *EventGroupProcessor) StreamFilter() []string {
	out := make([]string, 0, len(p.handlers))
	for name := range p.handlers {
		out = append(out, name)
	}
	sort.Strings(out) // deterministic order
	return out
}
