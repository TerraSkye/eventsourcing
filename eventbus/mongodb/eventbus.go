package mongodb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	cqrs "github.com/terraskye/eventsourcing"
)

var _ cqrs.EventBus = (*EventBus)(nil)

// EventBus is a MongoDB-backed event bus that polls the outbox collection to
// deliver events to subscribers. Each named subscription's position is
// persisted in the subscriptions collection so that it resumes from the
// correct outbox entry after a restart.
//
// Pass the same *mongo.Database used by EventStore so that both components
// read from and write to the same outbox and subscriptions collections.
// A pollInterval of 500ms is a reasonable starting point for most workloads.
type EventBus struct {
	outbox        *mongo.Collection
	subscriptions *mongo.Collection
	mu            sync.RWMutex
	subs          map[string]*subscriber
	closed        bool
	errs          chan error
	wg            sync.WaitGroup
	pollInterval  time.Duration
}

type subscriber struct {
	name    string
	opts    subscriberOptions
	handler cqrs.EventHandler
	cancel  context.CancelFunc
}

type subscriberOptions struct {
	filterEvents []string
	startFrom    *int64
}

// subscriptionDoc is the document shape in the subscriptions collection.
type subscriptionDoc struct {
	ID       string `bson:"_id"`
	Position int64  `bson:"position"`
}

// outboxRow is the shape of documents read from the outbox collection.
// It matches the outboxEntry written by the EventStore.
type outboxRow struct {
	GlobalPosition int64     `bson:"global_position"`
	StreamID       string    `bson:"stream_id"`
	EventID        string    `bson:"event_id"`
	StreamPosition int64     `bson:"stream_position"`
	EventType      string    `bson:"event_type"`
	Payload        []byte    `bson:"payload"`
	Metadata       []byte    `bson:"metadata"`
	OccurredAt     time.Time `bson:"occurred_at"`
}

// NewEventBus creates a new MongoDB-backed EventBus.
func NewEventBus(db *mongo.Database, pollInterval time.Duration) *EventBus {
	return &EventBus{
		outbox:        db.Collection("outbox"),
		subscriptions: db.Collection("subscriptions"),
		subs:          make(map[string]*subscriber),
		errs:          make(chan error, 64),
		pollInterval:  pollInterval,
	}
}

// Subscribe registers a named handler that consumes events from the outbox.
// If a subscription record for name already exists in the database its stored
// position is used, allowing the handler to continue from where it left off.
// WithStartFrom only takes effect when no existing record is found.
func (b *EventBus) Subscribe(ctx context.Context, name string, handler cqrs.EventHandler, opts ...cqrs.SubscriberOption) error {
	if handler == nil {
		return errors.New("handler cannot be nil")
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return errors.New("eventbus is closed")
	}
	if _, exists := b.subs[name]; exists {
		return fmt.Errorf("subscriber %q already exists", name)
	}

	subOpts := &subscriberOptions{}
	for _, o := range opts {
		o(subOpts)
	}

	workerCtx, cancel := context.WithCancel(context.Background())
	sub := &subscriber{
		name:    name,
		opts:    *subOpts,
		handler: handler,
		cancel:  cancel,
	}
	b.subs[name] = sub

	b.wg.Add(1)
	go b.runSubscriber(workerCtx, sub)

	// Cancel the subscriber when the caller's context is done.
	go func() {
		<-ctx.Done()
		b.removeSubscriber(name)
	}()

	return nil
}

// Errors returns a channel on which async handler errors are sent.
func (b *EventBus) Errors() <-chan error {
	return b.errs
}

// Close stops all active subscribers and waits for their goroutines to exit.
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

func (b *EventBus) runSubscriber(ctx context.Context, s *subscriber) {
	defer b.wg.Done()

	pos, err := b.getOrInitPosition(ctx, s)
	if err != nil {
		b.sendErr(fmt.Errorf("subscriber %q: init position: %w", s.name, err))
		return
	}

	ticker := time.NewTicker(b.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			newPos, err := b.poll(ctx, s, pos)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				b.sendErr(fmt.Errorf("subscriber %q: poll: %w", s.name, err))
				continue
			}
			pos = newPos
		}
	}
}

// getOrInitPosition creates a subscriptions document with the configured
// start position if one does not yet exist, then returns the stored position.
func (b *EventBus) getOrInitPosition(ctx context.Context, s *subscriber) (int64, error) {
	defaultPos := int64(0)
	if s.opts.startFrom != nil {
		defaultPos = *s.opts.startFrom
	}

	// $setOnInsert only applies when the document is inserted (upsert), so
	// existing positions are never overwritten on restart.
	if _, err := b.subscriptions.UpdateOne(ctx,
		bson.M{"_id": s.name},
		bson.M{"$setOnInsert": bson.M{"position": defaultPos}},
		options.Update().SetUpsert(true),
	); err != nil {
		return 0, fmt.Errorf("upsert subscription record: %w", err)
	}

	var doc subscriptionDoc
	if err := b.subscriptions.FindOne(ctx, bson.M{"_id": s.name}).Decode(&doc); err != nil {
		return 0, fmt.Errorf("read subscription position: %w", err)
	}
	return doc.Position, nil
}

// poll fetches up to 100 outbox entries with a global_position greater than
// fromPos, dispatches each one to the subscriber, then persists the new
// position. Returns the updated position.
func (b *EventBus) poll(ctx context.Context, s *subscriber, fromPos int64) (int64, error) {
	filter := bson.M{"global_position": bson.M{"$gt": fromPos}}
	if len(s.opts.filterEvents) > 0 {
		filter["event_type"] = bson.M{"$in": s.opts.filterEvents}
	}

	cursor, err := b.outbox.Find(ctx, filter,
		options.Find().
			SetSort(bson.D{{Key: "global_position", Value: 1}}).
			SetLimit(100),
	)
	if err != nil {
		return fromPos, err
	}
	defer cursor.Close(ctx)

	pos := fromPos
	for cursor.Next(ctx) {
		var row outboxRow
		if err := cursor.Decode(&row); err != nil {
			return pos, fmt.Errorf("decode outbox row: %w", err)
		}

		env, err := outboxRowToEnvelope(&row)
		if err != nil {
			return pos, err
		}

		if err := s.handler.Handle(cqrs.WithEnvelope(ctx, env), env.Event); err != nil {
			var skipped *cqrs.ErrSkippedEvent
			if !errors.As(err, &skipped) {
				b.sendErr(fmt.Errorf("subscriber %q: handle %s: %w", s.name, env.Event.EventType(), err))
			}
		}

		pos = row.GlobalPosition
	}

	if err := cursor.Err(); err != nil {
		return fromPos, err
	}

	if pos != fromPos {
		if err := b.updatePosition(ctx, s.name, pos); err != nil {
			return pos, fmt.Errorf("update position: %w", err)
		}
	}

	return pos, nil
}

func (b *EventBus) updatePosition(ctx context.Context, name string, pos int64) error {
	_, err := b.subscriptions.UpdateOne(ctx,
		bson.M{"_id": name},
		bson.M{"$set": bson.M{"position": pos}},
	)
	return err
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

func (b *EventBus) sendErr(err error) {
	select {
	case b.errs <- err:
	default:
	}
}

// WithStartFrom sets the global outbox position from which a new subscription
// begins consuming. This only applies when no existing subscription record is
// found in the database; established subscriptions always resume from their
// stored position.
func WithStartFrom(pos int64) cqrs.SubscriberOption {
	return func(cfg any) {
		opts, ok := cfg.(*subscriberOptions)
		if !ok {
			panic(fmt.Sprintf("WithStartFrom: expected *subscriberOptions, got %T", cfg))
		}
		opts.startFrom = &pos
	}
}

// WithFilterEvents restricts a subscriber to only receive events whose
// EventType matches one of the provided names.
func WithFilterEvents(types []string) cqrs.SubscriberOption {
	return func(cfg any) {
		opts, ok := cfg.(*subscriberOptions)
		if !ok {
			panic(fmt.Sprintf("WithFilterEvents: expected *subscriberOptions, got %T", cfg))
		}
		opts.filterEvents = types
	}
}

func outboxRowToEnvelope(row *outboxRow) (*cqrs.Envelope, error) {
	ev, err := cqrs.NewEventByName(row.EventType)
	if err != nil {
		return nil, fmt.Errorf("cannot create event %q: %w", row.EventType, err)
	}
	if err := json.Unmarshal(row.Payload, ev); err != nil {
		return nil, fmt.Errorf("cannot unmarshal event %q: %w", row.EventType, err)
	}

	var meta map[string]any
	if len(row.Metadata) > 0 {
		if err := json.Unmarshal(row.Metadata, &meta); err != nil {
			meta = make(map[string]any)
		}
	} else {
		meta = make(map[string]any)
	}

	eventID, err := uuid.Parse(row.EventID)
	if err != nil {
		return nil, fmt.Errorf("parse event ID %q: %w", row.EventID, err)
	}

	return &cqrs.Envelope{
		EventID:       eventID,
		StreamID:      row.StreamID,
		Event:         ev,
		Metadata:      meta,
		Version:       uint64(row.StreamPosition),
		GlobalVersion: uint64(row.GlobalPosition),
		OccurredAt:    row.OccurredAt,
	}, nil
}
