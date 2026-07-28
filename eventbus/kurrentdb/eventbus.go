package kurrentdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/kurrent-io/KurrentDB-Client-Go/kurrentdb"
	cqrs "github.com/terraskye/eventsourcing"
)

// EventBus is a KurrentDB-backed [cqrs.EventBus]. Each subscriber is backed
// by a persistent subscription to $all: Subscribe creates the subscription
// if it does not already exist and then consumes it with automatic
// reconnect and retry. Delivery is at-least-once: an event is acknowledged
// to KurrentDB only after the handler returns nil or [cqrs.ErrSkippedEvent],
// so a handler error leaves the event pending for redelivery.
type EventBus struct {
	db          *kurrentdb.Client
	subs        map[string]*subscriber
	mu          sync.RWMutex
	closed      bool
	errs        chan error
	wg          sync.WaitGroup
	buffer      uint64
	middlewares []cqrs.EventHandlerMiddleware
}

// subscriber is a single registered handler together with the persistent
// subscription options it was created with.
type subscriber struct {
	name    string
	opt     kurrentdb.SubscribeToPersistentSubscriptionOptions
	filter  func(cqrs.Event) bool
	handler cqrs.EventHandler
	cancel  context.CancelFunc
}

// NewEventBus creates a KurrentDB-backed [EventBus] using db as the
// underlying client.
//
// TODO: the buffer parameter is stored but never read anywhere in this
// package — there is no internal channel it sizes. Either wire it up (e.g.
// to size a delivery buffer) or remove it; as written it is dead
// configuration that misleads callers into thinking it affects behavior.
func NewEventBus(db *kurrentdb.Client, buffer uint64) *EventBus {
	return &EventBus{
		db:     db,
		subs:   make(map[string]*subscriber),
		errs:   make(chan error, 64),
		buffer: buffer,
	}
}

// Use adds middlewares that wrap every handler registered afterward via
// Subscribe. As required by [cqrs.EventBus], it must be called before
// Subscribe: middlewares added after a given subscriber is registered do
// not apply to that subscriber.
func (b *EventBus) Use(middlewares ...cqrs.EventHandlerMiddleware) {
	b.middlewares = append(b.middlewares, middlewares...)
}

// Subscribe registers handler under name as the consumer of a persistent
// subscription to $all called name, creating that subscription first if it
// does not already exist (see EnsurePersistentSubscription). Options such
// as WithStartAtZero, WithStartAt, WithFilterEvents, and WithFilterStream
// configure the subscription at creation time only; they have no effect on
// a subscription that already exists in KurrentDB. It returns an error if
// handler is nil, the bus is already closed, name is already registered, or
// the subscription could not be ensured. The subscription is removed from
// the bus automatically when ctx is canceled.
func (b *EventBus) Subscribe(ctx context.Context, name string, handler cqrs.EventHandler, opts ...cqrs.SubscriberOption) error {
	if handler == nil {
		return errors.New("filter and handler cannot be nil")
	}

	wrapped := handler
	for i := len(b.middlewares) - 1; i >= 0; i-- {
		wrapped = b.middlewares[i](wrapped)
	}
	handler = wrapped

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return errors.New("eventbus is closed")
	}
	if _, exists := b.subs[name]; exists {
		b.mu.Unlock()
		return fmt.Errorf("subscriber %q already exists", name)
	}

	workerCtx, cancel := context.WithCancel(context.Background())

	opt := kurrentdb.PersistentAllSubscriptionOptions{}

	for _, o := range opts {
		o(&opt)
	}

	if err := b.EnsurePersistentSubscription(ctx, name, opt); err != nil {
		cancel()
		b.errs <- err
		return err
	}

	sub := &subscriber{name: name, handler: handler, cancel: cancel, opt: kurrentdb.SubscribeToPersistentSubscriptionOptions{}}
	b.subs[name] = sub
	b.mu.Unlock()

	// Start subscription in a separate goroutine
	b.wg.Add(1)
	go b.runSubscriber(workerCtx, sub)

	// Remove subscriber when caller context is done
	go func() {
		<-ctx.Done()
		b.removeSubscriber(name)
	}()

	return nil
}

// EnsurePersistentSubscription creates the persistent subscription to $all
// called name, using opt, if it does not already exist. If a subscription
// by that name already exists, it is left untouched.
//
// TODO: if GetPersistentSubscriptionInfoToAll fails with anything other
// than kurrentdb.ErrorCodeResourceNotFound — including errors that aren't a
// *kurrentdb.Error at all, e.g. a network failure — this function silently
// returns nil instead of propagating the error or creating the
// subscription. Subscribe then proceeds as if the subscription were ready,
// so the real failure only surfaces later (if at all) as an async error
// from the persistent-subscription stream. Consider returning err in the
// non-not-found cases instead of swallowing it.
func (b *EventBus) EnsurePersistentSubscription(ctx context.Context, name string, opt kurrentdb.PersistentAllSubscriptionOptions) error {

	var kurrentErr *kurrentdb.Error

	if _, err := b.db.GetPersistentSubscriptionInfoToAll(ctx, name, kurrentdb.GetPersistentSubscriptionOptions{}); err != nil {

		if errors.As(err, &kurrentErr) {

			switch kurrentErr.Code() {

			case kurrentdb.ErrorCodeResourceNotFound:

				if err := b.db.CreatePersistentSubscriptionToAll(ctx, name, opt); err != nil {
					return err
				}

			}

		}

	}

	return nil
}

// runSubscriber runs s's persistent subscription with exponential-backoff
// reconnect on failure, up to 100 attempts. If the subscription still
// cannot be established after those attempts, it sends a final error on
// Errors() and returns, permanently stopping delivery for s; s's name
// remains registered until its context is canceled or the bus is closed, so
// it cannot be re-subscribed under the same name in the meantime.
func (b *EventBus) runSubscriber(ctx context.Context, s *subscriber) {
	defer b.wg.Done()

	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = 100 * time.Millisecond
	bo.MaxInterval = 10 * time.Second
	bo.MaxElapsedTime = 0 // Never stop due to elapsed time

	retryBackoff := backoff.WithMaxRetries(bo, 100)
	retryBackoff = backoff.WithContext(retryBackoff, ctx)

	err := backoff.Retry(func() error {
		if err := b.runSubscription(ctx, s); err != nil {
			select {
			case b.errs <- fmt.Errorf("subscriber %q: %w", s.name, err):
			default:
			}
			return err
		}
		// Subscription ended without error (context cancelled)
		return nil
	}, retryBackoff)

	if err != nil {
		select {
		case b.errs <- fmt.Errorf("subscriber %q: max retries exceeded: %w", s.name, err):
		default:
		}
	}
}

// runSubscription reads from s's persistent subscription until ctx is
// canceled or the subscription is dropped, decoding each event, dispatching
// it to s.handler, and acknowledging it to KurrentDB unless the handler
// returns an error other than [cqrs.ErrSkippedEvent] (in which case it is
// left unacknowledged for redelivery). It returns a non-nil error whenever
// the subscription ends for a reason other than ctx being canceled, so the
// caller can decide to reconnect.
func (b *EventBus) runSubscription(ctx context.Context, s *subscriber) error {
	stream, err := b.db.SubscribeToPersistentSubscriptionToAll(ctx, s.name, s.opt)
	if err != nil {
		return err
	}
	defer stream.Close()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		subscriptionEvent := stream.Recv()

		kEvent := subscriptionEvent.EventAppeared

		if subscriptionEvent.SubscriptionDropped != nil {
			return errors.New("subscription dropped, reconnecting")
		}

		if kEvent == nil {
			// these are system events
			// checkpoints/ catchups etc.
			continue
		}

		if kEvent.Event.Event.EventType[0] == '$' {
			stream.Ack(kEvent.Event)
			continue
		}
		// Convert KurrentDB event to cqrs.Event
		ev, err := cqrs.NewEventByName(kEvent.Event.Event.EventType)
		if err != nil {
			select {
			case b.errs <- fmt.Errorf("subscriber %q: cannot create event %q: %w", s.name, kEvent.Event.Event.EventType, err):
			default:
			}
			continue
		}

		// Unmarshal KurrentDB event data into cqrs.EventData
		if err := json.Unmarshal(kEvent.Event.Event.Data, ev); err != nil {
			select {
			case b.errs <- fmt.Errorf("subscriber %q: cannot unmarshal event %q: %w", s.name, kEvent.Event.Event.EventType, err):
			default:
			}
			continue
		}

		var metaData map[string]any

		// Unmarshal KurrentDB event data into cqrs.EventData
		if kEvent.Event.Event.UserMetadata != nil {
			if err := json.Unmarshal(kEvent.Event.Event.UserMetadata, &metaData); err != nil {
				select {
				case b.errs <- fmt.Errorf("subscriber %q: cannot unmarshal metadata %q: %w", s.name, kEvent.Event.Event.EventType, err):
				default:
				}
				continue
			}
		}

		envelope := &cqrs.Envelope{
			EventID:       kEvent.Event.Event.EventID, // or use kEvent.ID if available
			StreamID:      kEvent.Event.Event.StreamID,
			Metadata:      metaData,
			Event:         ev,
			Version:       kEvent.Event.Event.EventNumber,
			GlobalVersion: kEvent.Event.Event.Position.Commit,
			OccurredAt:    kEvent.Event.Event.CreatedDate,
		}

		if err := s.handler.Handle(cqrs.WithEnvelope(ctx, envelope), envelope.Event); err != nil {
			var skippedErr *cqrs.ErrSkippedEvent
			if !errors.As(err, &skippedErr) {
				select {
				case b.errs <- fmt.Errorf("subscriber %q: %w", s.name, err):
				default:
				}
				continue
			}
		}
		if err := stream.Ack(kEvent.Event); err != nil {
			select {
			case b.errs <- fmt.Errorf("subscriber %q: failed to ack %s: %w", s.name, kEvent.Event.Event.EventID, err):
			default:
			}
		}

	}
}

// removeSubscriber cancels and unregisters the named subscriber. It is a
// no-op if name is not registered.
func (b *EventBus) removeSubscriber(name string) {
	b.mu.Lock()
	sub, ok := b.subs[name]
	if ok {
		delete(b.subs, name)
		sub.cancel()
	}
	b.mu.Unlock()
}

// Errors returns the channel on which asynchronous handler and subscription
// errors are delivered, as required by [cqrs.EventBus].
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

	for _, sub := range b.subs {
		sub.cancel()
	}
	b.subs = nil
	b.mu.Unlock()

	b.wg.Wait()
	close(b.errs)
	return nil
}

// WithStartAtZero returns a [cqrs.SubscriberOption] that starts a newly
// created persistent subscription from the beginning of $all. It has no
// effect on a subscription that already exists in KurrentDB, and it panics
// if applied to a bus other than this package's [EventBus].
func WithStartAtZero() cqrs.SubscriberOption {
	return func(cfg any) {
		opts, ok := cfg.(*kurrentdb.PersistentAllSubscriptionOptions)
		if !ok {
			panic(fmt.Sprintf("WithFrom: expected *SubscribeToAllOptions, got %T", cfg))
		}
		opts.StartFrom = kurrentdb.Start{}
	}
}

// WithStartAt returns a [cqrs.SubscriberOption] that starts a newly created
// persistent subscription from the given commit/prepare position in $all.
// It has no effect on a subscription that already exists in KurrentDB, and
// it panics if applied to a bus other than this package's [EventBus].
func WithStartAt(revision uint64) cqrs.SubscriberOption {
	return func(cfg any) {
		opts, ok := cfg.(*kurrentdb.PersistentAllSubscriptionOptions)
		if !ok {
			panic(fmt.Sprintf("WithFrom: expected *SubscribeToAllOptions, got %T", cfg))
		}
		opts.StartFrom = kurrentdb.Position{
			Commit:  revision,
			Prepare: revision,
		}
	}
}

// WithFilterEvents returns a [cqrs.SubscriberOption] that configures a
// newly created persistent subscription's server-side filter to include
// only events whose KurrentDB event type is one of filteredEvents. It has
// no effect on a subscription that already exists in KurrentDB. Since a
// persistent subscription has a single filter, combine all desired names
// into one call rather than calling this more than once: applying it
// together with WithFilterStream, or applying either more than once, means
// only the last one applied takes effect. It panics if applied to a bus
// other than this package's [EventBus].
func WithFilterEvents(filteredEvents []string) cqrs.SubscriberOption {
	return func(cfg any) {
		opts, ok := cfg.(*kurrentdb.PersistentAllSubscriptionOptions)
		if !ok {
			panic(fmt.Sprintf("WithFilterEvents: expected *SubscribeToAllOptions, got %T", cfg))
		}
		opts.Filter = &kurrentdb.SubscriptionFilter{
			Type:  kurrentdb.EventFilterType,
			Regex: fmt.Sprintf("^(%s)$", strings.Join(filteredEvents, "|")),
		}
	}
}

// WithFilterStream returns a [cqrs.SubscriberOption] that configures a
// newly created persistent subscription's server-side filter to include
// only streams whose name starts with one of streams. It has no effect on
// a subscription that already exists in KurrentDB, and — as with
// WithFilterEvents — only the last filter option applied takes effect. It
// panics if applied to a bus other than this package's [EventBus].
func WithFilterStream(streams []string) cqrs.SubscriberOption {
	return func(cfg any) {
		opts, ok := cfg.(*kurrentdb.PersistentAllSubscriptionOptions)
		if !ok {
			panic(fmt.Sprintf("WithFilterStream: expected *PersistentAllSubscriptionOptions, got %T", cfg))
		}
		opts.Filter = &kurrentdb.SubscriptionFilter{
			Type:     kurrentdb.StreamFilterType,
			Prefixes: streams,
		}
	}
}
