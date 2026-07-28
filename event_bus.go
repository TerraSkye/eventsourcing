package eventsourcing

import "context"

// SubscriberOption configures a subscription registered via
// [EventBus.Subscribe]. Available options are implementation-specific; see,
// for example, the memory package's WithFilterEvents.
type SubscriberOption func(cfg any)

// EventBus lets [EventHandler]s subscribe to receive events as they occur,
// fanning each event out to every matching subscriber. How events are fed
// into the bus (typically as a side effect of an [EventStore] save) is
// left to the implementation.
type EventBus interface {
	// Subscribe registers handler under name to receive events, optionally
	// narrowed by options. It returns an error if handler is nil, name is
	// already registered, or the subscription otherwise could not be added
	// (for example, for a networked implementation).
	Subscribe(ctx context.Context, name string, handler EventHandler, options ...SubscriberOption) error

	// Use adds middlewares to be applied to every handler registered via
	// Subscribe afterward. It must be called before Subscribe, since
	// middleware is applied at subscribe time.
	Use(middlewares ...EventHandlerMiddleware)

	// Errors returns a channel on which asynchronous handling errors are
	// delivered.
	Errors() <-chan error

	// Close shuts down the EventBus and waits for all handlers to finish
	// processing events already delivered to them.
	Close() error
}
