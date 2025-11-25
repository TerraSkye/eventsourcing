package eventsourcing

import "context"

type SubscriberOption func(cfg any)

// EventBus is an EventHandler that distributes published events to all matching
// handlers that are registered, but only one of each type will handle the event.
// Events are not guaranteed to be handeled in order.
type EventBus interface {
	// AddHandler adds a handler for an event. Returns an error if either the
	// matcher or handler is nil, the handler is already added or there was some
	// other problem adding the handler (for networked handlers for example).
	Subscribe(ctx context.Context, name string, handler EventHandler, options ...SubscriberOption) error

	// Errors returns an error channel where async handling errors are sent.
	Errors() <-chan error

	// Close closes the EventBus and waits for all handlers to finish.
	Close() error
}
