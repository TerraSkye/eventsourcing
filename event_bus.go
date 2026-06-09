package eventsourcing

import "context"

type SubscriberOption func(cfg any)

// EventBus is an EventHandler that distributes published events to all matching
// handlers that are registered, but only one of each type will handle the event.
type EventBus interface {
	// Subscribe adds a handler for an event. Returns an error if either the
	// matcher or handler is nil, the handler is already added or there was some
	// other problem adding the handler (for networked handlers for example).
	Subscribe(ctx context.Context, name string, handler EventHandler, options ...SubscriberOption) error

	// Errors returns an error channel where async handling errors are sent.
	Errors() <-chan error

	// Close closes the EventBus and waits for all handlers to finish.
	Close() error
}

// eventHandlerMiddlewareHandler is the interface for internal storage of event
// handler middlewares. Both EventHandlerMiddleware and struct-based middlewares
// satisfy it.
type eventHandlerMiddlewareHandler interface {
	Middleware(next EventHandler) EventHandler
}

// EventHandlerMiddleware wraps an EventHandler, allowing pre/post processing of events.
type EventHandlerMiddleware func(next EventHandler) EventHandler

// Middleware implements eventHandlerMiddlewareHandler.
func (mw EventHandlerMiddleware) Middleware(next EventHandler) EventHandler {
	return mw(next)
}

type middlewareEventBus struct {
	bus         EventBus
	middlewares []eventHandlerMiddlewareHandler
}

// NewEventBusWithMiddleware returns an EventBus that wraps every handler passed
// to Subscribe with the given middlewares. The first middleware is the outermost
// wrapper (executes first for each event).
func NewEventBusWithMiddleware(bus EventBus, middlewares ...EventHandlerMiddleware) EventBus {
	m := &middlewareEventBus{bus: bus}
	for _, mw := range middlewares {
		m.middlewares = append(m.middlewares, mw)
	}
	return m
}

func (m *middlewareEventBus) Subscribe(ctx context.Context, name string, handler EventHandler, options ...SubscriberOption) error {
	wrapped := handler
	for i := len(m.middlewares) - 1; i >= 0; i-- {
		wrapped = m.middlewares[i].Middleware(wrapped)
	}
	return m.bus.Subscribe(ctx, name, wrapped, options...)
}

func (m *middlewareEventBus) Errors() <-chan error {
	return m.bus.Errors()
}

func (m *middlewareEventBus) Close() error {
	return m.bus.Close()
}

// useMiddleware adds a struct-based middleware to the chain.
func (m *middlewareEventBus) useMiddleware(mw eventHandlerMiddlewareHandler) {
	m.middlewares = append(m.middlewares, mw)
}
