package kurrentdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/kurrent-io/KurrentDB-Client-Go/kurrentdb"
	cqrs "github.com/terraskye/eventsourcing"
)

type EventBus struct {
	db     *kurrentdb.Client
	subs   map[string]*subscriber
	mu     sync.RWMutex
	closed bool
	errs   chan error
	wg     sync.WaitGroup
	buffer uint64
}

type subscriber struct {
	name    string
	opt     kurrentdb.SubscribeToPersistentSubscriptionOptions
	filter  func(cqrs.Event) bool
	handler cqrs.EventHandler
	cancel  context.CancelFunc
}

// NewEventBus creates a KurrentDB-backed event bus
func NewEventBus(db *kurrentdb.Client, buffer uint64) *EventBus {
	return &EventBus{
		db:     db,
		subs:   make(map[string]*subscriber),
		errs:   make(chan error, 64),
		buffer: buffer,
	}
}

func (b *EventBus) Subscribe(ctx context.Context, name string, handler cqrs.EventHandler, opts ...cqrs.SubscriberOption) error {
	if handler == nil {
		return errors.New("filter and handler cannot be nil")
	}

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

func (b *EventBus) runSubscriber(ctx context.Context, s *subscriber) {
	defer b.wg.Done()

	stream, err := b.db.SubscribeToPersistentSubscriptionToAll(ctx, s.name, s.opt)
	if err != nil {
		select {
		case b.errs <- fmt.Errorf("subscriber %q: %w", s.name, err):
		default:
		}
		return
	}
	defer stream.Close()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		subscriptionEvent := stream.Recv()

		kEvent := subscriptionEvent.EventAppeared

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
		if err := json.Unmarshal(kEvent.Event.Event.UserMetadata, &metaData); err != nil {
			select {
			case b.errs <- fmt.Errorf("subscriber %q: cannot unmarshal metadata %q: %w", s.name, kEvent.Event.Event.EventType, err):
			default:
			}
			continue
		}

		envelope := &cqrs.Envelope{
			EventID:    kEvent.Event.Event.EventID, // or use kEvent.ID if available
			StreamID:   kEvent.Event.Event.StreamID,
			Metadata:   metaData,
			Event:      ev,
			Version:    kEvent.Event.Event.Position.Commit,
			OccurredAt: kEvent.Event.Event.CreatedDate,
		}

		// out of date

		// readstream ( 0, 10)

		//if s.filter(envelope.Event) {
		if err := s.handler.Handle(cqrs.WithEnvelope(ctx, envelope), envelope.Event); err != nil {
			select {
			case b.errs <- fmt.Errorf("subscriber %q: %w", s.name, err):
			default:
			}
		} else {
			stream.Ack(kEvent.Event)
		}
		//}
	}
}

func (b *EventBus) removeSubscriber(name string) {
	b.mu.Lock()
	sub, ok := b.subs[name]
	if ok {
		delete(b.subs, name)
		sub.cancel()
	}
	b.mu.Unlock()
}

func (b *EventBus) Errors() <-chan error {
	return b.errs
}

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

func WithStartAtZero() cqrs.SubscriberOption {
	return func(cfg any) {
		opts, ok := cfg.(*kurrentdb.PersistentAllSubscriptionOptions)
		if !ok {
			panic(fmt.Sprintf("WithFrom: expected *SubscribeToAllOptions, got %T", cfg))
		}
		opts.StartFrom = kurrentdb.Start{}
	}
}

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

func WithFilterEvents(filteredEvents []string) cqrs.SubscriberOption {
	return func(cfg any) {
		opts, ok := cfg.(*kurrentdb.PersistentAllSubscriptionOptions)
		if !ok {
			panic(fmt.Sprintf("WithFilterEvents: expected *SubscribeToAllOptions, got %T", cfg))
		}
		opts.Filter = &kurrentdb.SubscriptionFilter{
			Type:     kurrentdb.EventFilterType,
			Prefixes: filteredEvents,
		}
	}
}

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
