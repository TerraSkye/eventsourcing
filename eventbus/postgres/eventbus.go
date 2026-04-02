package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	cqrs "github.com/terraskye/eventsourcing"
)

var _ cqrs.EventBus = (*EventBus)(nil)

// EventBus is a PostgreSQL-backed event bus that uses polling to deliver events
// to subscribers. Each subscriber's position is persisted in the event_subscriptions
// table, allowing subscriptions to resume after a restart.
type EventBus struct {
	pool         *pgxpool.Pool
	subs         map[string]*subscriber
	mu           sync.RWMutex
	closed       bool
	errs         chan error
	wg           sync.WaitGroup
	pollInterval time.Duration
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

// NewEventBus creates a PostgreSQL-backed EventBus.
// pollInterval controls how frequently each subscriber polls for new events.
// A value of 500ms is a reasonable default for most workloads.
func NewEventBus(pool *pgxpool.Pool, pollInterval time.Duration) *EventBus {
	return &EventBus{
		pool:         pool,
		subs:         make(map[string]*subscriber),
		errs:         make(chan error, 64),
		pollInterval: pollInterval,
	}
}

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

	go func() {
		<-ctx.Done()
		b.removeSubscriber(name)
	}()

	return nil
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

// getOrInitPosition inserts a new subscription record with the configured start
// position if one does not already exist, then returns the current position.
func (b *EventBus) getOrInitPosition(ctx context.Context, s *subscriber) (int64, error) {
	defaultPos := int64(0)
	if s.opts.startFrom != nil {
		defaultPos = *s.opts.startFrom
	}

	_, err := b.pool.Exec(ctx,
		"INSERT INTO event_subscriptions (name, position) VALUES ($1, $2) ON CONFLICT (name) DO NOTHING",
		s.name, defaultPos,
	)
	if err != nil {
		return 0, err
	}

	var pos int64
	if err := b.pool.QueryRow(ctx, "SELECT position FROM event_subscriptions WHERE name = $1", s.name).Scan(&pos); err != nil {
		return 0, err
	}
	return pos, nil
}

// poll fetches up to 100 events after fromPos, dispatches them to the subscriber,
// then persists the new position. Returns the updated position.
func (b *EventBus) poll(ctx context.Context, s *subscriber, fromPos int64) (int64, error) {
	query := `
		SELECT id, event_id, stream_id, stream_position, event_type, payload, metadata, occurred_at
		FROM events
		WHERE id > $1`
	args := []any{fromPos}

	if len(s.opts.filterEvents) > 0 {
		query += " AND event_type = ANY($2)"
		args = append(args, s.opts.filterEvents)
	}

	query += " ORDER BY id ASC LIMIT 100"

	rows, err := b.pool.Query(ctx, query, args...)
	if err != nil {
		return fromPos, err
	}
	defer rows.Close()

	pos := fromPos
	for rows.Next() {
		env, err := scanEnvelope(rows)
		if err != nil {
			return pos, err
		}

		if err := s.handler.Handle(cqrs.WithEnvelope(ctx, env), env.Event); err != nil {
			var skipped *cqrs.ErrSkippedEvent
			if !errors.As(err, &skipped) {
				b.sendErr(fmt.Errorf("subscriber %q: handle %s: %w", s.name, env.Event.EventType(), err))
			}
		}

		pos = int64(env.GlobalVersion)
	}

	if err := rows.Err(); err != nil {
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
	_, err := b.pool.Exec(ctx,
		"UPDATE event_subscriptions SET position = $1 WHERE name = $2",
		pos, name,
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

// WithStartFrom sets the global event position from which a new subscription
// begins consuming. Only applies when no existing record exists in
// event_subscriptions; established subscriptions resume from their stored position.
func WithStartFrom(pos int64) cqrs.SubscriberOption {
	return func(cfg any) {
		opts, ok := cfg.(*subscriberOptions)
		if !ok {
			panic(fmt.Sprintf("WithStartFrom: expected *subscriberOptions, got %T", cfg))
		}
		opts.startFrom = &pos
	}
}

// WithFilterEvents restricts the subscriber to only receive events whose
// event_type matches one of the provided names.
func WithFilterEvents(types []string) cqrs.SubscriberOption {
	return func(cfg any) {
		opts, ok := cfg.(*subscriberOptions)
		if !ok {
			panic(fmt.Sprintf("WithFilterEvents: expected *subscriberOptions, got %T", cfg))
		}
		opts.filterEvents = types
	}
}

func scanEnvelope(rows pgx.Rows) (*cqrs.Envelope, error) {
	var (
		globalID       int64
		rawUUID        pgtype.UUID
		streamID       string
		streamPosition int64
		eventType      string
		payload        []byte
		metadata       []byte
		occurredAt     time.Time
	)

	if err := rows.Scan(&globalID, &rawUUID, &streamID, &streamPosition, &eventType, &payload, &metadata, &occurredAt); err != nil {
		return nil, fmt.Errorf("scan envelope: %w", err)
	}

	ev, err := cqrs.NewEventByName(eventType)
	if err != nil {
		return nil, fmt.Errorf("cannot create event %q: %w", eventType, err)
	}

	if err := json.Unmarshal(payload, ev); err != nil {
		return nil, fmt.Errorf("cannot unmarshal event %q: %w", eventType, err)
	}

	var meta map[string]any
	if len(metadata) > 0 {
		if err := json.Unmarshal(metadata, &meta); err != nil {
			meta = make(map[string]any)
		}
	} else {
		meta = make(map[string]any)
	}

	return &cqrs.Envelope{
		EventID:       uuid.UUID(rawUUID.Bytes),
		StreamID:      streamID,
		Event:         ev,
		Metadata:      meta,
		Version:       uint64(streamPosition),
		GlobalVersion: uint64(globalID),
		OccurredAt:    occurredAt,
	}, nil
}
