//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/postgres"
)

// collectHandler returns a handler that appends received events to a slice,
// and a function to read the current snapshot of that slice.
func collectHandler() (eventsourcing.EventHandler, func() []eventsourcing.Event) {
	var mu sync.Mutex
	var received []eventsourcing.Event
	handler := eventsourcing.NewEventHandlerFunc(func(_ context.Context, event eventsourcing.Event) error {
		mu.Lock()
		received = append(received, event)
		mu.Unlock()
		return nil
	})
	snapshot := func() []eventsourcing.Event {
		mu.Lock()
		defer mu.Unlock()
		out := make([]eventsourcing.Event, len(received))
		copy(out, received)
		return out
	}
	return handler, snapshot
}

// waitFor polls until cond() returns true or timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// --- Tests ---

func TestSubscribe_ReceivesEvents(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewEventStore(pool)
	bus := postgres.NewEventBus(pool, 20*time.Millisecond)
	defer bus.Close() //nolint:errcheck

	handler, snapshot := collectHandler()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := bus.Subscribe(ctx, "sub-1", handler); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	_, err := store.Save(ctx, []eventsourcing.Envelope{
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1"}),
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1"}),
	}, eventsourcing.Any{})
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	if !waitFor(t, 2*time.Second, func() bool { return len(snapshot()) == 2 }) {
		t.Errorf("expected 2 events, got %d", len(snapshot()))
	}
}

func TestSubscribe_PositionPersisted(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewEventStore(pool)
	ctx := context.Background()

	// Save 2 events, then start a subscriber and let it consume them.
	_, _ = store.Save(ctx, []eventsourcing.Envelope{
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1"}),
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1"}),
	}, eventsourcing.Any{})

	bus1 := postgres.NewEventBus(pool, 20*time.Millisecond)
	handler1, snapshot1 := collectHandler()
	subCtx, cancel := context.WithCancel(context.Background())

	if err := bus1.Subscribe(subCtx, "persistent-sub", handler1); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if !waitFor(t, 2*time.Second, func() bool { return len(snapshot1()) == 2 }) {
		t.Fatalf("first bus: expected 2 events, got %d", len(snapshot1()))
	}

	// Shut down the first bus, simulating a restart.
	cancel()
	bus1.Close() //nolint:errcheck

	// Save a third event.
	_, _ = store.Save(ctx, []eventsourcing.Envelope{
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1"}),
	}, eventsourcing.Any{})

	// Start a new bus with the same subscriber name. It must resume from the
	// persisted position and only deliver the third event.
	bus2 := postgres.NewEventBus(pool, 20*time.Millisecond)
	defer bus2.Close() //nolint:errcheck
	handler2, snapshot2 := collectHandler()

	if err := bus2.Subscribe(context.Background(), "persistent-sub", handler2); err != nil {
		t.Fatalf("re-subscribe: %v", err)
	}
	if !waitFor(t, 2*time.Second, func() bool { return len(snapshot2()) == 1 }) {
		t.Errorf("expected 1 new event after restart, got %d", len(snapshot2()))
	}
}

// TestSubscribe_CloseDuringHandlerCommitsPosition covers Close landing
// while a handler is busy with the second of three events in one batch
// (#181): the handler finishes, the position of the two events handled is
// committed rather than rolled back, and the third is left for later.
func TestSubscribe_CloseDuringHandlerCommitsPosition(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewEventStore(pool)
	ctx := context.Background()
	if _, err := store.Save(ctx, []eventsourcing.Envelope{
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1"}),
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1"}),
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1"}),
	}, eventsourcing.Any{}); err != nil {
		t.Fatalf("save: %v", err)
	}

	var (
		mu      sync.Mutex
		seen    int
		reached = make(chan struct{})
	)
	handler := eventsourcing.NewEventHandlerFunc(func(ctx context.Context, _ eventsourcing.Event) error {
		mu.Lock()
		seen++
		n := seen
		mu.Unlock()
		if n == 2 {
			// Still busy with this event when Close lands.
			close(reached)
			<-ctx.Done()
		}
		return nil
	})
	bus := postgres.NewEventBus(pool, 20*time.Millisecond)
	if err := bus.Subscribe(ctx, "closing-sub", handler); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	select {
	case <-reached:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never reached the second event")
	}
	bus.Close() //nolint:errcheck

	var pos int64
	if err := pool.QueryRow(ctx, "SELECT position FROM event_subscriptions WHERE name = 'closing-sub'").Scan(&pos); err != nil {
		t.Fatalf("read position: %v", err)
	}
	if pos != 2 {
		t.Fatalf("stored position after Close = %d, want 2: the events the handler finished", pos)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen != 2 {
		t.Fatalf("handler received %d events, want 2: none after Close", seen)
	}
}

// TestSubscribe_IgnoresInFlightTransactions verifies that the eventbus poller
// never delivers an event that would skip a gap left by an uncommitted
// transaction.
//
// Scenario:
//   - TX1 (raw): inserts stream-A event (gets id=1), stays open
//   - TX2 (store.Save): inserts stream-B event (gets id=2), commits
//   - After multiple poll cycles the subscriber must have received nothing
//   - TX1 commits → subscriber eventually receives both events in order
func TestSubscribe_IgnoresInFlightTransactions(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewEventStore(pool)
	bus := postgres.NewEventBus(pool, 20*time.Millisecond)
	defer bus.Close() //nolint:errcheck

	handler, snapshot := collectHandler()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := bus.Subscribe(ctx, "gap-sub", handler); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Acquire a dedicated connection and begin TX1 without committing.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire conn: %v", err)
	}
	defer conn.Release()

	tx, err := conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}

	payload, _ := json.Marshal(&OrderCreated{OrderID: "A"})
	_, err = tx.Exec(ctx, `
		INSERT INTO events (event_id, stream_id, stream_position, event_type, payload, metadata, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		uuid.New(), "stream-A", 1, "OrderCreated", payload, []byte("{}"), time.Now(),
	)
	if err != nil {
		tx.Rollback(ctx) //nolint:errcheck
		t.Fatalf("raw insert: %v", err)
	}

	// Commit stream-B via the store (id=2).
	_, err = store.Save(ctx, []eventsourcing.Envelope{newEnvelope("stream-B", &OrderCreated{OrderID: "B"})}, eventsourcing.Any{})
	if err != nil {
		tx.Rollback(ctx) //nolint:errcheck
		t.Fatalf("store.Save: %v", err)
	}

	// Allow several poll cycles. The subscriber must not receive anything
	// because id=1 (stream-A) is still in-flight.
	time.Sleep(150 * time.Millisecond)
	if got := len(snapshot()); got != 0 {
		tx.Rollback(ctx) //nolint:errcheck
		t.Errorf("expected 0 events while TX1 in-flight, got %d", got)
	}

	// Commit TX1.
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit tx: %v", err)
	}

	// Now both events must arrive in global id order.
	if !waitFor(t, time.Second, func() bool { return len(snapshot()) == 2 }) {
		t.Fatalf("expected 2 events after TX1 commits, got %d", len(snapshot()))
	}
	events := snapshot()
	if events[0].(*OrderCreated).OrderID != "A" {
		t.Errorf("expected stream-A event first, got OrderID=%s", events[0].(*OrderCreated).OrderID)
	}
	if events[1].(*OrderCreated).OrderID != "B" {
		t.Errorf("expected stream-B event second, got OrderID=%s", events[1].(*OrderCreated).OrderID)
	}
}

// TestSubscribe_NotifyWakesImmediately verifies that a subscriber receives a
// newly saved event almost immediately via LISTEN/NOTIFY, without waiting for
// its (here, deliberately very long) pollInterval ticker.
func TestSubscribe_NotifyWakesImmediately(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewEventStore(pool)
	bus := postgres.NewEventBus(pool, 10*time.Second)
	defer bus.Close() //nolint:errcheck

	handler, snapshot := collectHandler()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := bus.Subscribe(ctx, "notify-sub", handler); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Wait for the bus's LISTEN goroutine to establish its connection
	// before the insert, so the NOTIFY isn't missed.
	select {
	case <-bus.Listening():
	case <-time.After(2 * time.Second):
		t.Fatal("listen connection was not established in time")
	}

	if _, err := store.Save(ctx, []eventsourcing.Envelope{
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1"}),
	}, eventsourcing.Any{}); err != nil {
		t.Fatalf("save: %v", err)
	}

	// With a 10s pollInterval, this can only pass via NOTIFY.
	if !waitFor(t, 2*time.Second, func() bool { return len(snapshot()) == 1 }) {
		t.Errorf("expected 1 event delivered via NOTIFY within 2s, got %d", len(snapshot()))
	}
}

// TestSubscribe_ManySubscribersSmallPool_NoDeadlock reproduces a deadlock
// where poll() held a connection from the same pool used for handler writes:
// once 2*activeSubscribers > pool.MaxConns, every poll() and every Handle()
// call permanently blocked waiting for a connection. With 13 subscribers and
// MaxConns=4, the first tick after Subscribe used to wedge the bus forever.
func TestSubscribe_ManySubscribersSmallPool_NoDeadlock(t *testing.T) {
	ctx := context.Background()

	cfg, err := pgxpool.ParseConfig(testDSN)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.MaxConns = 4

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect pool: %v", err)
	}
	if _, err := pool.Exec(ctx, "TRUNCATE events RESTART IDENTITY; TRUNCATE event_subscriptions"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(pool.Close)

	store := postgres.NewEventStore(pool)
	bus := postgres.NewEventBus(pool, 10*time.Millisecond)
	defer bus.Close() //nolint:errcheck

	const numSubscribers = 20
	var received [numSubscribers]atomic.Int32

	for i := range numSubscribers {
		idx := i
		// Simulate a projection write: Handle() acquires its own connection
		// from the same pool poll() uses.
		handler := eventsourcing.NewEventHandlerFunc(func(ctx context.Context, _ eventsourcing.Event) error {
			if _, err := pool.Exec(ctx, "SELECT pg_sleep(0.01)"); err != nil {
				return err
			}
			received[idx].Add(1)
			return nil
		})
		if err := bus.Subscribe(ctx, fmt.Sprintf("sub-%d", idx), handler); err != nil {
			t.Fatalf("subscribe %d: %v", idx, err)
		}
	}

	if _, err := store.Save(ctx, []eventsourcing.Envelope{
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1"}),
	}, eventsourcing.Any{}); err != nil {
		t.Fatalf("save: %v", err)
	}

	if !waitFor(t, 5*time.Second, func() bool {
		for i := range numSubscribers {
			if received[i].Load() == 0 {
				return false
			}
		}
		return true
	}) {
		t.Fatalf("not all %d subscribers received the event within timeout (deadlock?)", numSubscribers)
	}
}

// TestUse_RacesWithSubscribe is a regression test for GitHub issue #35: Use
// appended to b.middlewares with no synchronization at all, and Subscribe
// read b.middlewares before acquiring b.mu, so calling Use concurrently with
// Subscribe was a data race under `go test -race`.
func TestUse_RacesWithSubscribe(t *testing.T) {
	pool := newPool(t)
	bus := postgres.NewEventBus(pool, 50*time.Millisecond)
	t.Cleanup(func() { _ = bus.Close() })

	handler := eventsourcing.NewEventHandlerFunc(func(ctx context.Context, event eventsourcing.Event) error {
		return nil
	})

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			bus.Use(func(next eventsourcing.EventHandler) eventsourcing.EventHandler {
				return next
			})
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			// Same name every time: only the first call actually registers a
			// subscriber, but every call reads b.middlewares before that
			// check, which is enough to race against the writer above.
			_ = bus.Subscribe(context.Background(), "sub", handler)
		}
	}()

	wg.Wait()
}

func countGoroutinesCreatedBy(substr string) int {
	buf := make([]byte, 4<<20)
	n := runtime.Stack(buf, true)
	return strings.Count(string(buf[:n]), substr)
}

// TestSubscribe_CtxWatcherGoroutineLeaksAfterClose is a regression test for
// GitHub issue #34: the goroutine that auto-removes a subscriber blocked on
// <-ctx.Done() alone, which never fires for a long-lived ctx such as
// context.Background() (used by every other test/example in this repo) —
// leaking one goroutine per Subscribe call for the rest of the process's
// life, unaffected by Close.
func TestSubscribe_CtxWatcherGoroutineLeaksAfterClose(t *testing.T) {
	const n = 20
	const createdBy = "created by github.com/terraskye/eventsourcing/postgres.(*EventBus).Subscribe"

	pool := newPool(t)
	bus := postgres.NewEventBus(pool, 50*time.Millisecond)

	handler := eventsourcing.NewEventHandlerFunc(func(ctx context.Context, event eventsourcing.Event) error {
		return nil
	})

	before := countGoroutinesCreatedBy(createdBy)

	for i := 0; i < n; i++ {
		name := "leak-sub-" + strconv.Itoa(i)
		if err := bus.Subscribe(context.Background(), name, handler); err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
	}

	if err := bus.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Give any well-behaved goroutines a chance to exit.
	time.Sleep(500 * time.Millisecond)
	runtime.GC()

	after := countGoroutinesCreatedBy(createdBy)

	leaked := after - before
	if leaked > 0 {
		t.Fatalf("expected 0 leaked ctx-watcher goroutines after Close, got %d (before=%d after=%d)", leaked, before, after)
	}
}
