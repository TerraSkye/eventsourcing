package memory

import (
	"context"
	"errors"
	"fmt"
	"sync"

	cqrs "github.com/terraskye/eventsourcing"
)

type subscriber struct {
	name    string
	filter  func(cqrs.Event) bool
	handler cqrs.EventHandler
	events  chan cqrs.Event
	cancel  context.CancelFunc
}

type eventBus struct {
	mu         sync.RWMutex
	subs       map[string]*subscriber
	closed     bool
	errs       chan error
	wg         sync.WaitGroup
	bufferSize int
}

// NewEventBus constructs a new bus with a given subscriber buffer size.
func NewEventBus(bufferSize int) cqrs.EventBus {
	return &eventBus{
		subs:       make(map[string]*subscriber),
		errs:       make(chan error, 64),
		bufferSize: bufferSize,
	}
}

// Subscribe registers a handler with a filter and name.
func (b *eventBus) Subscribe(
	ctx context.Context,
	name string,
	filter func(cqrs.Event) bool,
	handler cqrs.EventHandler,
	opts ...cqrs.SubscriberOption,
) error {
	if filter == nil || handler == nil {
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
		filter:  filter,
		handler: handler,
		//TODO change cqrs.event to cqrs.Envelope
		events: make(chan cqrs.Event, b.bufferSize),
		cancel: cancel,
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

func (b *eventBus) Errors() <-chan error {
	return b.errs
}

// Close shuts down the bus and waits for all workers.
func (b *eventBus) Close() error {
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
func (b *eventBus) runSubscriber(ctx context.Context, s *subscriber) {
	defer b.wg.Done()

	for {
		select {
		case <-ctx.Done():
			return

		case ev, ok := <-s.events:
			if !ok {
				return
			}

			// todo extract event envelope
			// todo add envelope content into ctx

			// Handle event
			if err := s.handler.Handle(ctx, ev); err != nil {
				select {
				case b.errs <- fmt.Errorf("handler %q: %w", s.name, err):
				default:
					// Drop error if channel full
				}
			}
		}
	}
}

func (b *eventBus) removeSubscriber(name string) {
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
func (b *eventBus) Dispatch(ev *cqrs.Envelope) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return
	}

	for _, s := range b.subs {
		if s.filter(ev.Event) {
			select {
			case s.events <- ev.Event:
			default:
				// Drop event if subscriber is busy
			}
		}
	}
}
