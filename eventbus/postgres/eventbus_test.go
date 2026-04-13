package postgres_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"log"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	cqrs "github.com/terraskye/eventsourcing"
	pgbus "github.com/terraskye/eventsourcing/eventbus/postgres"
	pgstore "github.com/terraskye/eventsourcing/eventstore/postgres"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

//go:embed schema.sql
var busSchema string

// events table schema — kept in sync with eventstore/postgres/schema.sql
const storeSchema = `
CREATE TABLE IF NOT EXISTS events (
    id              BIGSERIAL    PRIMARY KEY,
    event_id        UUID         NOT NULL,
    stream_id       VARCHAR      NOT NULL,
    stream_position BIGINT       NOT NULL,
    event_type      VARCHAR      NOT NULL,
    payload         BYTEA        NOT NULL,
    metadata        BYTEA,
    occurred_at     TIMESTAMPTZ  NOT NULL,
    UNIQUE (stream_id, stream_position)
);
CREATE INDEX IF NOT EXISTS idx_events_stream_id ON events (stream_id);
`

// --- Test event type ---

type OrderCreated struct {
	OrderID string
}

func (e *OrderCreated) AggregateID() string { return e.OrderID }
func (e *OrderCreated) EventType() string   { return "OrderCreated" }

// --- Package-level state ---

var testDSN string

func TestMain(m *testing.M) {
	cqrs.RegisterEventByType(func() cqrs.Event { return &OrderCreated{} })

	ctx := context.Background()
	pgc, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("testdb"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
	)
	if err != nil {
		log.Fatalf("start postgres container: %v", err)
	}
	defer pgc.Terminate(ctx) //nolint:errcheck

	testDSN, err = pgc.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Fatalf("get connection string: %v", err)
	}

	pool, err := pgxpool.New(ctx, testDSN)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	// pgxpool.New is lazy; ping with retries to wait for the container to be
	// fully accepting connections (it can reset connections briefly after the
	// "ready" log line appears).
	for i := range 20 {
		if err = pool.Ping(ctx); err == nil {
			break
		}
		if i == 19 {
			log.Fatalf("ping postgres: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if _, err = pool.Exec(ctx, storeSchema); err != nil {
		log.Fatalf("apply store schema: %v", err)
	}
	if _, err = pool.Exec(ctx, busSchema); err != nil {
		log.Fatalf("apply bus schema: %v", err)
	}
	pool.Close()

	os.Exit(m.Run())
}

// --- Helpers ---

func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("connect pool: %v", err)
	}
	_, err = pool.Exec(ctx, "TRUNCATE events RESTART IDENTITY; TRUNCATE event_subscriptions")
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func newEnvelope(streamID string, event cqrs.Event) cqrs.Envelope {
	return cqrs.Envelope{
		EventID:    uuid.New(),
		StreamID:   streamID,
		Event:      event,
		OccurredAt: time.Now(),
		Metadata:   map[string]any{},
	}
}

// collectHandler returns a handler that appends received events to a slice,
// and a function to read the current snapshot of that slice.
func collectHandler() (cqrs.EventHandler, func() []cqrs.Event) {
	var mu sync.Mutex
	var received []cqrs.Event
	handler := cqrs.NewEventHandlerFunc(func(_ context.Context, event cqrs.Event) error {
		mu.Lock()
		received = append(received, event)
		mu.Unlock()
		return nil
	})
	snapshot := func() []cqrs.Event {
		mu.Lock()
		defer mu.Unlock()
		out := make([]cqrs.Event, len(received))
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
	store := pgstore.NewEventStore(pool)
	bus := pgbus.NewEventBus(pool, 20*time.Millisecond)
	defer bus.Close() //nolint:errcheck

	handler, snapshot := collectHandler()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := bus.Subscribe(ctx, "sub-1", handler); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	_, err := store.Save(ctx, []cqrs.Envelope{
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1"}),
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1"}),
	}, cqrs.Any{})
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	if !waitFor(t, 2*time.Second, func() bool { return len(snapshot()) == 2 }) {
		t.Errorf("expected 2 events, got %d", len(snapshot()))
	}
}

func TestSubscribe_PositionPersisted(t *testing.T) {
	pool := newPool(t)
	store := pgstore.NewEventStore(pool)
	ctx := context.Background()

	// Save 2 events, then start a subscriber and let it consume them.
	_, _ = store.Save(ctx, []cqrs.Envelope{
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1"}),
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1"}),
	}, cqrs.Any{})

	bus1 := pgbus.NewEventBus(pool, 20*time.Millisecond)
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
	_, _ = store.Save(ctx, []cqrs.Envelope{
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1"}),
	}, cqrs.Any{})

	// Start a new bus with the same subscriber name. It must resume from the
	// persisted position and only deliver the third event.
	bus2 := pgbus.NewEventBus(pool, 20*time.Millisecond)
	defer bus2.Close() //nolint:errcheck
	handler2, snapshot2 := collectHandler()

	if err := bus2.Subscribe(context.Background(), "persistent-sub", handler2); err != nil {
		t.Fatalf("re-subscribe: %v", err)
	}
	if !waitFor(t, 2*time.Second, func() bool { return len(snapshot2()) == 1 }) {
		t.Errorf("expected 1 new event after restart, got %d", len(snapshot2()))
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
	store := pgstore.NewEventStore(pool)
	bus := pgbus.NewEventBus(pool, 20*time.Millisecond)
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
	_, err = store.Save(ctx, []cqrs.Envelope{newEnvelope("stream-B", &OrderCreated{OrderID: "B"})}, cqrs.Any{})
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
