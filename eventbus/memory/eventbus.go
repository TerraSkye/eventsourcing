package memory

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	cqrs "github.com/terraskye/eventsourcing"
)

// subscriber is a single registered handler and its per-handler delivery
// state: an event-type filter and a buffered channel served by its own
// worker goroutine.
type subscriber struct {
	name    string
	handler cqrs.EventHandler
	filter  *filter
	events  chan *cqrs.Envelope
	cancel  context.CancelFunc
}

// filter holds the event-type allowlist configured via [WithFilterEvents].
type filter struct {
	events []string
}

// EventBus is an in-memory [cqrs.EventBus]. It delivers events to
// subscribers within the process, using one buffered channel and worker
// goroutine per subscriber; it does not persist events, so nothing is
// replayed across process restarts.
type EventBus struct {
	mu          sync.RWMutex
	subs        map[string]*subscriber
	closed      bool
	errs        chan error
	wg          sync.WaitGroup
	bufferSize  int
	middlewares []cqrs.EventHandlerMiddleware
}

// NewEventBus constructs an [EventBus] whose subscribers each buffer up to
// bufferSize undelivered events. Once a subscriber's buffer is full,
// Dispatch blocks until that subscriber's handler drains it.
func NewEventBus(bufferSize int) *EventBus {
	return &EventBus{
		subs:       make(map[string]*subscriber),
		errs:       make(chan error, 64),
		bufferSize: bufferSize,
	}
}

// Use adds middlewares that wrap every handler registered afterward via
// Subscribe. As required by [cqrs.EventBus], it must be called before
// Subscribe: middlewares added after a given subscriber is registered do
// not apply to that subscriber. Use and Subscribe are both safe to call
// concurrently.
func (b *EventBus) Use(middlewares ...cqrs.EventHandlerMiddleware) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.middlewares = append(b.middlewares, middlewares...)
}

// Subscribe registers handler under name to receive dispatched events. By
// default a subscriber receives every event; pass [WithFilterEvents] to
// restrict delivery to specific event types. It returns an error if handler
// is nil, the bus is already closed, or name is already registered. The
// subscription is removed automatically when ctx is canceled.
func (b *EventBus) Subscribe(
	ctx context.Context,
	name string,
	handler cqrs.EventHandler,
	opts ...cqrs.SubscriberOption,
) error {
	if handler == nil {
		return errors.New("filter and handler cannot be nil")
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return errors.New("eventbus is closed")
	}

	if _, exists := b.subs[name]; exists {
		return fmt.Errorf("handler with name %q already registered", name)
	}

	wrapped := handler
	for i := len(b.middlewares) - 1; i >= 0; i-- {
		wrapped = b.middlewares[i](wrapped)
	}
	handler = wrapped

	workerCtx, cancel := context.WithCancel(context.Background())
	s := &subscriber{
		name:    name,
		handler: handler,
		filter: &filter{
			events: make([]string, 0),
		},
		events: make(chan *cqrs.Envelope, b.bufferSize),
		cancel: cancel,
	}

	for _, opt := range opts {
		opt(s.filter)
	}

	b.subs[name] = s

	// Start worker
	b.wg.Add(1)
	go b.runSubscriber(workerCtx, s)

	// Automatically remove when caller’s ctx finishes
	go func() {
		<-ctx.Done()
		b.removeSubscriber(name)
	}()

	return nil
}

// Errors returns the channel on which asynchronous handler errors are
// delivered, as required by [cqrs.EventBus].
func (b *EventBus) Errors() <-chan error {
	return b.errs
}

// Close cancels every subscriber and waits for their worker goroutines to
// finish before closing the Errors channel. Calling Close more than once is
// a no-op.
func (b *EventBus) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true

	// Close all subscribers
	for name, s := range b.subs {
		s.cancel()
		close(s.events)
		delete(b.subs, name)
	}
	b.mu.Unlock()

	// Wait until all workers finish
	b.wg.Wait()

	// Close error channel
	close(b.errs)

	return nil
}

// runSubscriber delivers events from s.events to s.handler until ctx is
// canceled or s.events is closed by Close or removeSubscriber.
func (b *EventBus) runSubscriber(ctx context.Context, s *subscriber) {
	defer b.wg.Done()

	for {
		select {
		case <-ctx.Done():
			return

		case ev, ok := <-s.events:
			if !ok {
				return
			}

			// Handle event
			if err := s.handler.Handle(cqrs.WithEnvelope(ctx, ev), ev.Event); err != nil {
				select {
				case b.errs <- fmt.Errorf("handler %q: %w", s.name, err):
				default:
					// Drop error if channel full
				}
			}
		}
	}
}

// removeSubscriber cancels and unregisters the named subscriber. It is a
// no-op if name is not registered.
func (b *EventBus) removeSubscriber(name string) {
	b.mu.Lock()
	s, ok := b.subs[name]
	if !ok {
		b.mu.Unlock()
		return
	}
	delete(b.subs, name)
	b.mu.Unlock()

	s.cancel()
	close(s.events)
}

// Dispatch delivers ev to every subscriber whose event-type filter matches
// it (see [WithFilterEvents]), or to every subscriber if none configured a
// filter. Each subscriber's handler sees events in the order Dispatch
// delivered them, on that subscriber's own goroutine, but Dispatch itself
// blocks until ev is queued for every matching subscriber — a subscriber
// whose buffer is full therefore delays the caller until its handler
// catches up. Dispatch is a no-op once the bus has been closed.
func (b *EventBus) Dispatch(ev *cqrs.Envelope) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return
	}

	for _, s := range b.subs {

		if len(s.filter.events) == 0 || slices.Contains(s.filter.events, ev.Event.EventType()) {
			select {
			case s.events <- ev:
			}
		}
	}
}

// WithFilterEvents returns a [cqrs.SubscriberOption] that restricts delivery
// to events whose [cqrs.Event.EventType] matches one of filteredEvents.
// Without this option, a subscriber receives every dispatched event. It
// panics if applied to a bus other than this package's [EventBus].
func WithFilterEvents(filteredEvents []string) cqrs.SubscriberOption {
	return func(cfg any) {
		opts, ok := cfg.(*filter)
		if !ok {
			panic(fmt.Sprintf("WithFilterEvents: expected *filter, got %T", cfg))
		}

		opts.events = filteredEvents
	}
}
