package fixtures

import (
	"context"
	"sync"

	es "github.com/terraskye/eventsourcing"
)

// EventBusSpy is a configurable mock EventBus for testing.
// It tracks subscriptions and allows injecting custom behavior.
type EventBusSpy struct {
	mu sync.Mutex

	// Function overrides
	SubscribeFn func(ctx context.Context, name string, handler es.EventHandler, options ...es.SubscriberOption) error
	ErrorsFn    func() <-chan error
	CloseFn     func() error

	// Call tracking
	SubscribeCalls int
	CloseCalls     int

	// Captured subscriptions
	Subscriptions []Subscription

	// Error injection
	subscribeErr error
	errChan      chan error
	closed       bool
}

// Subscription captures details of a Subscribe call.
type Subscription struct {
	Name    string
	Handler es.EventHandler
}

// NewEventBusSpy creates a new EventBusSpy.
func NewEventBusSpy() *EventBusSpy {
	return &EventBusSpy{
		errChan: make(chan error, 10),
	}
}

// FailOnSubscribe configures the bus to return an error on Subscribe.
func (b *EventBusSpy) FailOnSubscribe(err error) *EventBusSpy {
	b.subscribeErr = err
	return b
}

// Subscribe implements EventBus.Subscribe.
func (b *EventBusSpy) Subscribe(ctx context.Context, name string, handler es.EventHandler, options ...es.SubscriberOption) error {
	b.mu.Lock()
	b.SubscribeCalls++
	b.Subscriptions = append(b.Subscriptions, Subscription{
		Name:    name,
		Handler: handler,
	})
	b.mu.Unlock()

	if b.SubscribeFn != nil {
		return b.SubscribeFn(ctx, name, handler, options...)
	}

	if b.subscribeErr != nil {
		return b.subscribeErr
	}

	return nil
}

// Errors implements EventBus.Errors.
func (b *EventBusSpy) Errors() <-chan error {
	if b.ErrorsFn != nil {
		return b.ErrorsFn()
	}
	return b.errChan
}

// Close implements EventBus.Close.
func (b *EventBusSpy) Close() error {
	b.mu.Lock()
	b.CloseCalls++
	if !b.closed {
		b.closed = true
		close(b.errChan)
	}
	b.mu.Unlock()

	if b.CloseFn != nil {
		return b.CloseFn()
	}
	return nil
}

// SendError sends an error to the error channel for testing error handling.
func (b *EventBusSpy) SendError(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.closed {
		b.errChan <- err
	}
}

// Reset clears all call counts and subscriptions.
func (b *EventBusSpy) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.SubscribeCalls = 0
	b.CloseCalls = 0
	b.Subscriptions = nil
	b.subscribeErr = nil
}

// HasSubscription checks if a subscription with the given name exists.
func (b *EventBusSpy) HasSubscription(name string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, sub := range b.Subscriptions {
		if sub.Name == name {
			return true
		}
	}
	return false
}

// SubscriptionCount returns the number of subscriptions.
func (b *EventBusSpy) SubscriptionCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.Subscriptions)
}

// EventHandlerSpy is a configurable mock EventHandler for testing.
type EventHandlerSpy struct {
	mu sync.Mutex

	// Function override
	HandleFn func(ctx context.Context, event es.Event) error

	// Call tracking
	HandleCalls int

	// Captured events
	ReceivedEvents []es.Event

	// Error injection
	handleErr error
}

// NewEventHandlerSpy creates a new EventHandlerSpy.
func NewEventHandlerSpy() *EventHandlerSpy {
	return &EventHandlerSpy{}
}

// FailOnHandle configures the handler to return an error.
func (h *EventHandlerSpy) FailOnHandle(err error) *EventHandlerSpy {
	h.handleErr = err
	return h
}

// Handle implements EventHandler.Handle.
func (h *EventHandlerSpy) Handle(ctx context.Context, event es.Event) error {
	h.mu.Lock()
	h.HandleCalls++
	h.ReceivedEvents = append(h.ReceivedEvents, event)
	h.mu.Unlock()

	if h.HandleFn != nil {
		return h.HandleFn(ctx, event)
	}

	if h.handleErr != nil {
		return h.handleErr
	}

	return nil
}

// Reset clears all call counts and received events.
func (h *EventHandlerSpy) Reset() {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.HandleCalls = 0
	h.ReceivedEvents = nil
	h.handleErr = nil
}

// LastEvent returns the most recently received event, or nil if none.
func (h *EventHandlerSpy) LastEvent() es.Event {
	h.mu.Lock()
	defer h.mu.Unlock()

	if len(h.ReceivedEvents) == 0 {
		return nil
	}
	return h.ReceivedEvents[len(h.ReceivedEvents)-1]
}

// EventCount returns the number of events received.
func (h *EventHandlerSpy) EventCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.ReceivedEvents)
}
