package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	cqrs "github.com/terraskye/eventsourcing"
)

var _ cqrs.EventBus = (*EventBus)(nil)

// lockPoolMaxConns bounds the size of the dedicated pool used for poll()'s
// subscription-locking transaction.
const lockPoolMaxConns = 4

// notifyChannel is the Postgres NOTIFY channel that the trigger installed by
// schema.sql sends on after every insert into events.
const notifyChannel = "eventsourcing_events_inserted"

// EventBus is a PostgreSQL-backed [cqrs.EventBus] that delivers events by
// polling, woken promptly by LISTEN/NOTIFY (see NewEventBus). Each
// subscriber's position is persisted in the event_subscriptions table, so
// subscriptions resume where they left off after a restart. Delivery is
// at-least-once and in strict id order: if a handler returns an error other
// than [cqrs.ErrSkippedEvent], that event and the rest of its batch are
// retried on the next poll without advancing the subscriber's position (see
// poll), so a handler that keeps failing blocks that subscriber
// indefinitely.
type EventBus struct {
	pool         *pgxpool.Pool
	lockPool     *pgxpool.Pool
	subs         map[string]*subscriber
	mu           sync.RWMutex
	closed       bool
	errs         chan error
	wg           sync.WaitGroup
	pollInterval time.Duration
	middlewares  []cqrs.EventHandlerMiddleware
	listenCancel context.CancelFunc
	listenReady  chan struct{}
	listenOnce   sync.Once
}

// subscriber is a single registered handler and its poll state: its
// options, and a wake channel used to trigger an immediate poll on NOTIFY.
type subscriber struct {
	name    string
	opts    subscriberOptions
	handler cqrs.EventHandler
	cancel  context.CancelFunc
	wake    chan struct{}
}

// subscriberOptions accumulates the [cqrs.SubscriberOption] values passed
// to Subscribe.
type subscriberOptions struct {
	filterEvents []string
	startFrom    *int64
}

// NewEventBus creates a PostgreSQL-backed [EventBus].
// pollInterval controls how frequently each subscriber polls for new events
// as a fallback; in normal operation, LISTEN/NOTIFY wakes subscribers
// immediately after a commit. A value of a few seconds is reasonable for
// most workloads.
func NewEventBus(pool *pgxpool.Pool, pollInterval time.Duration) *EventBus {
	listenCtx, cancel := context.WithCancel(context.Background())
	b := &EventBus{
		pool:         pool,
		lockPool:     newLockPool(pool),
		subs:         make(map[string]*subscriber),
		errs:         make(chan error, 64),
		pollInterval: pollInterval,
		listenCancel: cancel,
		listenReady:  make(chan struct{}),
	}

	b.wg.Add(1)
	go b.listen(listenCtx)

	return b
}

// newLockPool creates a small pool dedicated to poll()'s subscription-locking
// transaction.
func newLockPool(pool *pgxpool.Pool) *pgxpool.Pool {
	cfg := pool.Config().Copy()
	cfg.MaxConns = lockPoolMaxConns
	cfg.MinConns = 0
	cfg.MinIdleConns = 0

	lockPool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		// cfg is derived from an already-validated pool config, so this
		// should be unreachable; fall back to sharing pool, which
		// re-introduces the deadlock risk poll() is designed to avoid.
		slog.Warn("eventbus: failed to create dedicated lock pool, poll() will share the main pool", "error", err)
		return pool
	}
	return lockPool
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

// Subscribe registers handler under name and starts polling for events on
// its behalf, creating a persisted position record in event_subscriptions
// if name has not been seen before (see ensureSubscription). By default a
// subscriber receives every event; pass [WithFilterEvents] to restrict
// delivery to specific event types, and [WithStartFrom] to control where a
// brand-new subscription begins. It returns an error if handler is nil, the
// bus is already closed, or name is already registered. The subscription is
// removed automatically when ctx is canceled.
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

	wrapped := handler
	for i := len(b.middlewares) - 1; i >= 0; i-- {
		wrapped = b.middlewares[i](wrapped)
	}
	handler = wrapped

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
		wake:    make(chan struct{}, 1),
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

// Errors returns the channel on which asynchronous handler and polling
// errors are delivered, as required by [cqrs.EventBus].
func (b *EventBus) Errors() <-chan error {
	return b.errs
}

// Close stops the LISTEN connection and every subscriber's polling, waits
// for their goroutines to finish, and closes the Errors channel. Calling
// Close more than once is a no-op.
func (b *EventBus) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	b.listenCancel()

	for _, sub := range b.subs {
		sub.cancel()
	}
	b.subs = nil
	b.mu.Unlock()

	b.wg.Wait()
	close(b.errs)

	if b.lockPool != b.pool {
		b.lockPool.Close()
	}
	return nil
}

// runSubscriber polls for s on b.pollInterval, or immediately whenever s.wake
// is signaled by a NOTIFY, until ctx is canceled.
func (b *EventBus) runSubscriber(ctx context.Context, s *subscriber) {
	defer b.wg.Done()

	if err := b.ensureSubscription(ctx, s); err != nil {
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
			if err := b.poll(ctx, s); err != nil {
				if ctx.Err() != nil {
					return
				}
				b.sendErr(fmt.Errorf("subscriber %q: poll: %w", s.name, err))
			}
		case <-s.wake:
			if err := b.poll(ctx, s); err != nil {
				if ctx.Err() != nil {
					return
				}
				b.sendErr(fmt.Errorf("subscriber %q: poll: %w", s.name, err))
			}
			ticker.Reset(b.pollInterval)
		}
	}
}

// ensureSubscription inserts a subscription record if one does not already exist.
func (b *EventBus) ensureSubscription(ctx context.Context, s *subscriber) error {
	defaultPos := int64(0)
	if s.opts.startFrom != nil {
		defaultPos = *s.opts.startFrom
	}
	_, err := b.pool.Exec(ctx,
		"INSERT INTO event_subscriptions (name, position) VALUES ($1, $2) ON CONFLICT (name) DO NOTHING",
		s.name, defaultPos,
	)
	return err
}

// poll locks the subscription row so that only one instance processes
// events for s at a time, then reads events with id greater than the
// stored position and delivers them to s.handler in id order.
//
// The scan only considers events already visible in a fresh MVCC snapshot,
// filtering out rows whose inserting transaction is still in progress (via
// pg_snapshot_xmin). This means a slow transaction that has already
// allocated a lower id delays delivery of every higher id until it commits
// or rolls back, which keeps poll from ever advancing past that id and
// permanently skipping it once it does commit.
//
// If s.handler returns an error other than [cqrs.ErrSkippedEvent], poll
// returns without committing, so the position is not advanced and the same
// batch (including any events already handled successfully earlier in this
// call) is retried on the next poll.
//
// The locking transaction runs on b.lockPool, not b.pool: it is held open for the
// entire call, including every s.handler.Handle() invocation, and Handle() typically
// needs its own connection from b.pool. Keeping the two pools separate avoids a
// deadlock between poll()'s held connections and handlers' connections.
func (b *EventBus) poll(ctx context.Context, s *subscriber) error {
	tx, err := b.lockPool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var pos int64
	err = tx.QueryRow(ctx,
		"SELECT position FROM event_subscriptions WHERE name = $1 FOR UPDATE SKIP LOCKED",
		s.name,
	).Scan(&pos)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // another instance holds the lock; skip this cycle
	}
	if err != nil {
		return err
	}

	query := `
		SELECT id, event_id, stream_id, stream_position, event_type, payload, metadata, occurred_at
		FROM events
		WHERE id > $1
		  AND xmin::text::bigint < pg_snapshot_xmin(pg_current_snapshot())::text::bigint`
	args := []any{pos}

	if len(s.opts.filterEvents) > 0 {
		query += " AND event_type = ANY($2)"
		args = append(args, s.opts.filterEvents)
	}
	query += " ORDER BY id ASC LIMIT 100"

	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		env, err := scanEnvelope(rows)
		if err != nil {
			return err
		}

		if err := s.handler.Handle(cqrs.WithEnvelope(ctx, env), env.Event); err != nil {
			var skipped *cqrs.ErrSkippedEvent
			if !errors.As(err, &skipped) {
				return fmt.Errorf("handle %s: %w", env.Event.EventType(), err)
			}
		}

		pos = int64(env.GlobalVersion)
	}

	if err := rows.Err(); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx,
		"UPDATE event_subscriptions SET position = $1 WHERE name = $2",
		pos, s.name,
	); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// listenReconnectDelay is how long listen waits before retrying after the
// LISTEN connection is lost or fails to establish.
const listenReconnectDelay = 5 * time.Second

// listen holds a dedicated connection LISTENing on notifyChannel and wakes
// every subscriber whenever a notification arrives. It is purely an
// optimization: if the connection cannot be established or drops, poll()'s
// pollInterval ticker continues to deliver events on its own, just less
// promptly. listen reconnects automatically until ctx is cancelled.
func (b *EventBus) listen(ctx context.Context) {
	defer b.wg.Done()

	for ctx.Err() == nil {
		conn, err := pgx.ConnectConfig(ctx, b.pool.Config().ConnConfig)
		if err != nil {
			b.sendErr(fmt.Errorf("eventbus: listen connect: %w", err))
			if !sleepCtx(ctx, listenReconnectDelay) {
				return
			}
			continue
		}

		if _, err := conn.Exec(ctx, "LISTEN "+notifyChannel); err != nil {
			conn.Close(context.Background()) //nolint:errcheck
			b.sendErr(fmt.Errorf("eventbus: listen %s: %w", notifyChannel, err))
			if !sleepCtx(ctx, listenReconnectDelay) {
				return
			}
			continue
		}

		b.listenOnce.Do(func() { close(b.listenReady) })

		for {
			if _, err := conn.WaitForNotification(ctx); err != nil {
				conn.Close(context.Background()) //nolint:errcheck
				if ctx.Err() != nil {
					return
				}
				b.sendErr(fmt.Errorf("eventbus: wait for notification: %w", err))
				break
			}
			b.wakeAll()
		}

		if !sleepCtx(ctx, listenReconnectDelay) {
			return
		}
	}
}

// Listening returns a channel that is closed once the bus's dedicated
// LISTEN connection has been established and is actively waiting for
// notifications. It is primarily useful in tests that need to avoid a race
// where a NOTIFY fires before the listen goroutine has started listening.
func (b *EventBus) Listening() <-chan struct{} {
	return b.listenReady
}

// wakeAll signals every active subscriber to poll immediately.
func (b *EventBus) wakeAll() {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, s := range b.subs {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}

// sleepCtx waits for d, returning false early if ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
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

// sendErr sends err on the Errors channel, dropping it if the channel's
// buffer is full rather than blocking the caller.
func (b *EventBus) sendErr(err error) {
	select {
	case b.errs <- err:
	default:
	}
}

// WithStartFrom returns a [cqrs.SubscriberOption] that sets the global event
// position from which a new subscription begins consuming. It only applies
// when name has no existing record in event_subscriptions; an already
// established subscription resumes from its stored position regardless. It
// panics if applied to a bus other than this package's [EventBus].
func WithStartFrom(pos int64) cqrs.SubscriberOption {
	return func(cfg any) {
		opts, ok := cfg.(*subscriberOptions)
		if !ok {
			panic(fmt.Sprintf("WithStartFrom: expected *subscriberOptions, got %T", cfg))
		}
		opts.startFrom = &pos
	}
}

// WithFilterEvents returns a [cqrs.SubscriberOption] that restricts the
// subscriber to events whose event_type matches one of types. Without this
// option, a subscriber receives every event. It panics if applied to a bus
// other than this package's [EventBus].
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
