package eventsourcing

import (
	"context"
)

type HydrateHandler interface {
	NewEvent() Event
	Apply(ctx context.Context, event Event)
}

type genericHydrateHandler[T Event] struct {
	handleFunc func(ctx context.Context, event T)
}

// NewEventHandler creates a new EventHandler implementation based on provided function
// and event type inferred from function argument.
func NewHydrateHandler[T Event](
	handleFunc func(ctx context.Context, event T),
) HydrateHandler {
	return &genericHydrateHandler[T]{
		handleFunc: handleFunc,
	}
}

func (c genericHydrateHandler[T]) NewEvent() Event {
	tVar := new(T)
	return *tVar
}

func (c genericHydrateHandler[T]) Apply(ctx context.Context, e Event) {
	event := e.(T)
	c.handleFunc(ctx, event)
}

func Hydrate(handlers ...HydrateHandler) func(ctx context.Context, ev Event) {
	eventHandlers := make(map[string]HydrateHandler)

	for _, handler := range handlers {
		eventHandlers[TypeName(handler.NewEvent())] = handler
	}

	return func(ctx context.Context, ev Event) {
		eventName := TypeName(ev)
		if handler, ok := eventHandlers[eventName]; ok {
			handler.Apply(ctx, ev)
		}
	}
}
