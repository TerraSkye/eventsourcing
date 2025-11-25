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

type eventBus struct {
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
	opt     kurrentdb.SubscribeToAllOptions
	filter  func(cqrs.Event) bool
	handler cqrs.EventHandler
	cancel  context.CancelFunc
}

// NewEventBus creates a KurrentDB-backed event bus
func NewEventBus(db *kurrentdb.Client, buffer uint64) cqrs.EventBus {
	return &eventBus{
		db:     db,
		subs:   make(map[string]*subscriber),
		errs:   make(chan error, 64),
		buffer: buffer,
	}
}

func (b *eventBus) Subscribe(ctx context.Context, name string, handler cqrs.EventHandler, opts ...cqrs.SubscriberOption) error {
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

	opt := kurrentdb.SubscribeToAllOptions{}

	for _, o := range opts {
		o(&opt)
	}

	sub := &subscriber{name: name, handler: handler, cancel: cancel, opt: opt}
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

func (b *eventBus) runSubscriber(ctx context.Context, s *subscriber) {
	defer b.wg.Done()

	stream, err := b.db.SubscribeToAll(ctx, s.opt)
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

		if err != nil {
			select {
			case b.errs <- fmt.Errorf("subscriber %q: %w", s.name, err):
			default:
			}
			return
		}

		kEvent := subscriptionEvent.EventAppeared

		// Convert KurrentDB event to cqrs.Envelope
		ev, err := cqrs.NewEventByName(kEvent.Event.EventType)
		if err != nil {
			select {
			case b.errs <- fmt.Errorf("subscriber %q: cannot create event %q: %w", s.name, kEvent.Event.EventType, err):
			default:
			}
			continue
		}

		// Unmarshal KurrentDB event data into cqrs.Event
		if err := json.Unmarshal(kEvent.Event.Data, ev); err != nil {
			select {
			case b.errs <- fmt.Errorf("subscriber %q: cannot unmarshal event %q: %w", s.name, kEvent.Event.EventType, err):
			default:
			}
			continue
		}

		var metaData map[string]any

		// Unmarshal KurrentDB event data into cqrs.Event
		if err := json.Unmarshal(kEvent.Event.UserMetadata, &metaData); err != nil {
			select {
			case b.errs <- fmt.Errorf("subscriber %q: cannot unmarshal metadata %q: %w", s.name, kEvent.Event.EventType, err):
			default:
			}
			continue
		}

		envelope := &cqrs.Envelope{
			EventID:    kEvent.Event.EventID, // or use kEvent.ID if available
			StreamID:   kEvent.Event.StreamID,
			Metadata:   metaData,
			Event:      ev,
			Version:    kEvent.Event.Position.Commit,
			OccurredAt: kEvent.Event.CreatedDate,
		}

		if s.filter(envelope.Event) {
			if err := s.handler.Handle(cqrs.WithEnvelope(ctx, envelope), envelope.Event); err != nil {
				select {
				case b.errs <- fmt.Errorf("subscriber %q: %w", s.name, err):
				default:
				}
			}
		}
	}
}

func (b *eventBus) removeSubscriber(name string) {
	b.mu.Lock()
	sub, ok := b.subs[name]
	if ok {
		delete(b.subs, name)
		sub.cancel()
	}
	b.mu.Unlock()
}

func (b *eventBus) Errors() <-chan error {
	return b.errs
}

func (b *eventBus) Close() error {
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

func WithFromStart() cqrs.SubscriberOption {
	return func(cfg any) {
		opts, ok := cfg.(*kurrentdb.SubscribeToAllOptions)
		if !ok {
			panic(fmt.Sprintf("WithFrom: expected *SubscribeToAllOptions, got %T", cfg))
		}
		opts.From = kurrentdb.Start{}
	}
}

func WithFilterEvents(filteredEvents []string) cqrs.SubscriberOption {
	return func(cfg any) {
		opts, ok := cfg.(*kurrentdb.SubscribeToAllOptions)
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
		opts, ok := cfg.(*kurrentdb.SubscribeToAllOptions)
		if !ok {
			panic(fmt.Sprintf("WithFilterStream: expected *SubscribeToAllOptions, got %T", cfg))
		}
		opts.Filter = &kurrentdb.SubscriptionFilter{
			Type:     kurrentdb.StreamFilterType,
			Prefixes: streams,
		}
	}
}
