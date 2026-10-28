package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/terraskye/eventsourcing"
)

var _ eventsourcing.EventBus = (*EventBus)(nil)

// busBatchSize is the most events a subscriber reads per poll.
const busBatchSize = 100

// EventBus is a SQLite-backed [eventsourcing.EventBus] that delivers events by
// polling the events table. Each subscriber's position is persisted in the
// event_subscriptions table, so a subscription resumes where it left off
// after a restart. Create one with [NewEventBus].
//
// Delivery is at-least-once and in id order: when a handler returns an
// error other than [eventsourcing.SkippedEventError], the subscriber's position
// stays before that event and the event is delivered again on the next
// poll, so a handler that keeps failing blocks its subscriber. No
// transaction is held while handlers run, so they may use the database.
// Two processes subscribed under the same name on one file each deliver
// every event; the bus is meant for one process.
type EventBus struct {
	db           *sql.DB
	pollInterval time.Duration

	mu          sync.RWMutex
	subs        map[string]*busSubscriber
	middlewares []eventsourcing.EventHandlerMiddleware
	closed      bool

	errs chan error
	wg   sync.WaitGroup
}

// busSubscriber is a single registered handler and its options.
type busSubscriber struct {
	name    string
	opts    subscriberOptions
	handler eventsourcing.EventHandler
	cancel  context.CancelFunc
}

// subscriberOptions accumulates the [eventsourcing.SubscriberOption] values passed
// to Subscribe.
type subscriberOptions struct {
	filterEvents []string
	startFrom    *int64
}

// NewEventBus returns an [EventBus] on db, which must hold the tables
// [Schema] creates. Each subscriber reads new events every pollInterval;
// in a single process a few hundred milliseconds keeps delivery prompt.
func NewEventBus(db *sql.DB, pollInterval time.Duration) *EventBus {
	return &EventBus{
		db:           db,
		pollInterval: pollInterval,
		subs:         make(map[string]*busSubscriber),
		errs:         make(chan error, 64),
	}
}

// Use adds middlewares that wrap every handler registered afterward via
// Subscribe. As required by [eventsourcing.EventBus], it must be called before
// Subscribe.
func (b *EventBus) Use(middlewares ...eventsourcing.EventHandlerMiddleware) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.middlewares = append(b.middlewares, middlewares...)
}

// Subscribe registers handler under name and starts polling for events on
// its behalf, creating a persisted position for name if it has none yet.
// By default a subscriber receives every event; pass [WithFilterEvents] to
// restrict delivery to some event types, and [WithStartFrom] to choose
// where a new subscription begins. It returns an error if handler is nil,
// the bus is closed, or name is already registered. The subscription ends
// when ctx is canceled or the bus is closed.
func (b *EventBus) Subscribe(ctx context.Context, name string, handler eventsourcing.EventHandler, opts ...eventsourcing.SubscriberOption) error {
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

	for i := len(b.middlewares) - 1; i >= 0; i-- {
		handler = b.middlewares[i](handler)
	}
	subOpts := &subscriberOptions{}
	for _, o := range opts {
		o(subOpts)
	}

	workerCtx, cancel := context.WithCancel(ctx)
	sub := &busSubscriber{name: name, opts: *subOpts, handler: handler, cancel: cancel}
	b.subs[name] = sub

	b.wg.Add(1)
	go b.runSubscriber(workerCtx, sub)
	return nil
}

// Errors returns the channel on which asynchronous handler and polling
// errors are delivered, as required by [eventsourcing.EventBus].
func (b *EventBus) Errors() <-chan error {
	return b.errs
}

// Close stops every subscriber, waits for their goroutines to finish, and
// closes the Errors channel. It does not close the *sql.DB. Calling Close
// more than once is a no-op.
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

// runSubscriber polls for s every b.pollInterval until ctx is canceled, and
// then unregisters s.
func (b *EventBus) runSubscriber(ctx context.Context, s *busSubscriber) {
	defer b.wg.Done()
	defer b.removeSubscriber(s)

	if err := b.ensureSubscription(ctx, s); err != nil {
		b.sendErr(fmt.Errorf("subscriber %q: init position: %w", s.name, err))
		return
	}

	ticker := time.NewTicker(b.pollInterval)
	defer ticker.Stop()
	for {
		if err := b.poll(ctx, s); err != nil && ctx.Err() == nil {
			b.sendErr(fmt.Errorf("subscriber %q: poll: %w", s.name, err))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// busStatementGrace is how long the bus's own statements may still run
// once a subscriber's ctx is done; see detach.
const busStatementGrace = 5 * time.Second

// detach returns the context the bus runs its own statements under for a
// subscriber whose context is ctx. Close cancels ctx, but a statement the
// bus has started must still finish: the position write for an event the
// handler already handled, or the event is delivered again; and a
// cancelled statement makes database/sql drop its connection, which with
// the single connection Open keeps throws a ":memory:" database away. The
// returned context ends busStatementGrace after ctx does, so a hung
// database cannot hold Close forever. Call cancel when done.
func detach(ctx context.Context) (context.Context, context.CancelFunc) {
	dctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(ctx, func() { time.AfterFunc(busStatementGrace, cancel) })
	return dctx, func() {
		stop()
		cancel()
	}
}

// ensureSubscription inserts a position for s if it has none.
func (b *EventBus) ensureSubscription(ctx context.Context, s *busSubscriber) error {
	start := int64(0)
	if s.opts.startFrom != nil {
		start = *s.opts.startFrom
	}
	dctx, cancel := detach(ctx)
	defer cancel()
	_, err := b.db.ExecContext(dctx,
		"INSERT INTO event_subscriptions (name, position) VALUES (?, ?) ON CONFLICT (name) DO NOTHING",
		s.name, start,
	)
	return err
}

// poll delivers the events after s's position, in id order, and moves the
// position past each event its handler accepted. It stops at the first
// handler failure, leaving the position before the failing event, and
// before the next event once ctx is done. The handlers get ctx; the bus's
// own statements run under detach(ctx), so once a handler has handled an
// event its position is stored even when Close lands meanwhile.
func (b *EventBus) poll(ctx context.Context, s *busSubscriber) error {
	dctx, cancel := detach(ctx)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var pos int64
		if err := b.db.QueryRowContext(dctx,
			"SELECT position FROM event_subscriptions WHERE name = ?", s.name,
		).Scan(&pos); err != nil {
			return err
		}

		query := selectEvents + " WHERE id > ?"
		args := []any{pos}
		if len(s.opts.filterEvents) > 0 {
			query += " AND event_type IN (" + placeholders(len(s.opts.filterEvents)) + ")"
			for _, t := range s.opts.filterEvents {
				args = append(args, t)
			}
		}
		query += " ORDER BY id LIMIT ?"
		args = append(args, busBatchSize)

		// Read the whole batch first, so the connection is free while the
		// handlers run.
		envs, err := queryEnvelopes(dctx, b.db, query, args...)
		if err != nil {
			return err
		}

		for _, env := range envs {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := s.handler.Handle(eventsourcing.WithEnvelope(ctx, env), env.Event); err != nil {
				var skipped *eventsourcing.SkippedEventError
				if !errors.As(err, &skipped) {
					return fmt.Errorf("handle %s: %w", env.Event.EventType(), err)
				}
			}
			if _, err := b.db.ExecContext(dctx,
				"UPDATE event_subscriptions SET position = ? WHERE name = ? AND position < ?",
				int64(env.GlobalPosition), s.name, int64(env.GlobalPosition),
			); err != nil {
				return err
			}
		}
		if len(envs) < busBatchSize {
			return nil
		}
	}
}

// removeSubscriber unregisters s if it is still the registered subscriber
// under its name.
func (b *EventBus) removeSubscriber(s *busSubscriber) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs[s.name] == s {
		delete(b.subs, s.name)
	}
	s.cancel()
}

// sendErr sends err on the Errors channel, dropping it if the channel's
// buffer is full rather than blocking the caller.
func (b *EventBus) sendErr(err error) {
	select {
	case b.errs <- err:
	default:
	}
}

// WithStartFrom returns a [eventsourcing.SubscriberOption] that sets the global
// position after which a new subscription begins. It only applies when the
// subscription has no stored position yet. It panics if applied to a bus
// other than this package's [EventBus].
func WithStartFrom(pos int64) eventsourcing.SubscriberOption {
	return func(cfg any) {
		opts, ok := cfg.(*subscriberOptions)
		if !ok {
			panic(fmt.Sprintf("WithStartFrom: expected *subscriberOptions, got %T", cfg))
		}
		opts.startFrom = &pos
	}
}

// WithFilterEvents returns a [eventsourcing.SubscriberOption] that restricts the
// subscriber to events whose type is one of types. Without it, a
// subscriber receives every event. It panics if applied to a bus other
// than this package's [EventBus].
func WithFilterEvents(types []string) eventsourcing.SubscriberOption {
	return func(cfg any) {
		opts, ok := cfg.(*subscriberOptions)
		if !ok {
			panic(fmt.Sprintf("WithFilterEvents: expected *subscriberOptions, got %T", cfg))
		}
		opts.filterEvents = types
	}
}
