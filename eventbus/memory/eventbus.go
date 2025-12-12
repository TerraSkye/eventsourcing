package memory

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	cqrs "github.com/terraskye/eventsourcing"
)

type subscriber struct {
	name    string
	handler cqrs.EventHandler
	filter  *filter
	events  chan *cqrs.Envelope
	cancel  context.CancelFunc
}

type filter struct {
	events []string
}

type EventBus struct {
	mu         sync.RWMutex
	subs       map[string]*subscriber
	closed     bool
	errs       chan error
	wg         sync.WaitGroup
	bufferSize int
}

// NewEventBus constructs a new bus with a given subscriber buffer size.
func NewEventBus(bufferSize int) *EventBus {
	return &EventBus{
		subs:       make(map[string]*subscriber),
		errs:       make(chan error, 64),
		bufferSize: bufferSize,
	}
}

// Subscribe registers a handler with a filter and name.
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

func (b *EventBus) Errors() <-chan error {
	return b.errs
}

// Close shuts down the bus and waits for all workers.
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

// runSubscriber processes events for a single handler.
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

// Dispatch sends an event to all matching subscribers.
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

func WithFilterEvents(filteredEvents []string) cqrs.SubscriberOption {
	return func(cfg any) {
		opts, ok := cfg.(*filter)
		if !ok {
			panic(fmt.Sprintf("WithFilterEvents: expected *filter, got %T", cfg))
		}

		opts.events = filteredEvents
	}
}
