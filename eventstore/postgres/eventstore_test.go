package postgres_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	cqrs "github.com/terraskye/eventsourcing"
	pgstore "github.com/terraskye/eventsourcing/eventstore/postgres"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

//go:embed schema.sql
var schemaSql string

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
	if _, err = pool.Exec(ctx, schemaSql); err != nil {
		log.Fatalf("apply schema: %v", err)
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
	if _, err = pool.Exec(ctx, "TRUNCATE events RESTART IDENTITY"); err != nil {
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

func collectAll(t *testing.T, iter *cqrs.Iterator[*cqrs.Envelope]) []*cqrs.Envelope {
	t.Helper()
	ctx := context.Background()
	var results []*cqrs.Envelope
	for iter.Next(ctx) {
		results = append(results, iter.Value())
	}
	if err := iter.Err(); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("iterator error: %v", err)
	}
	return results
}

// --- Tests ---

func TestSave_AndLoadStream(t *testing.T) {
	pool := newPool(t)
	store := pgstore.NewEventStore(pool)

	ctx := context.Background()
	events := []cqrs.Envelope{
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1"}),
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1"}),
	}

	result, err := store.Save(ctx, events, cqrs.NoStream{})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if !result.Successful {
		t.Fatal("expected successful result")
	}
	if result.NextExpectedVersion != 2 {
		t.Errorf("expected next version 2, got %d", result.NextExpectedVersion)
	}

	iter, err := store.LoadStream(ctx, "order-1")
	if err != nil {
		t.Fatalf("load stream: %v", err)
	}
	loaded := collectAll(t, iter)
	if len(loaded) != 2 {
		t.Errorf("expected 2 events, got %d", len(loaded))
	}
}

func TestSave_RevisionConflict(t *testing.T) {
	pool := newPool(t)
	store := pgstore.NewEventStore(pool)
	ctx := context.Background()

	_, err := store.Save(ctx, []cqrs.Envelope{
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1"}),
	}, cqrs.Any{})
	if err != nil {
		t.Fatalf("first save: %v", err)
	}

	_, err = store.Save(ctx, []cqrs.Envelope{
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1"}),
	}, cqrs.Revision(0)) // wrong revision
	if err == nil {
		t.Fatal("expected revision conflict error")
	}
	var conflictErr *cqrs.StreamRevisionConflictError
	if !errors.As(err, &conflictErr) {
		t.Errorf("expected StreamRevisionConflictError, got %T: %v", err, err)
	}
}

func TestSave_NoStream_FailsWhenStreamExists(t *testing.T) {
	pool := newPool(t)
	store := pgstore.NewEventStore(pool)
	ctx := context.Background()

	_, _ = store.Save(ctx, []cqrs.Envelope{newEnvelope("order-1", &OrderCreated{OrderID: "order-1"})}, cqrs.Any{})

	_, err := store.Save(ctx, []cqrs.Envelope{newEnvelope("order-1", &OrderCreated{OrderID: "order-1"})}, cqrs.NoStream{})
	if !errors.Is(err, cqrs.ErrStreamExists) {
		t.Errorf("expected ErrStreamExists, got %v", err)
	}
}

func TestLoadStream_NotFound(t *testing.T) {
	pool := newPool(t)
	store := pgstore.NewEventStore(pool)

	_, err := store.LoadStream(context.Background(), "no-such-stream")
	if !errors.Is(err, cqrs.ErrStreamNotFound) {
		t.Errorf("expected ErrStreamNotFound, got %v", err)
	}
}

func TestLoadFromAll_BasicOrdering(t *testing.T) {
	pool := newPool(t)
	store := pgstore.NewEventStore(pool)
	ctx := context.Background()

	_, _ = store.Save(ctx, []cqrs.Envelope{newEnvelope("stream-A", &OrderCreated{OrderID: "A"})}, cqrs.Any{})
	_, _ = store.Save(ctx, []cqrs.Envelope{newEnvelope("stream-B", &OrderCreated{OrderID: "B"})}, cqrs.Any{})
	_, _ = store.Save(ctx, []cqrs.Envelope{newEnvelope("stream-A", &OrderCreated{OrderID: "A"})}, cqrs.Any{})

	iter, err := store.LoadFromAll(ctx, cqrs.Revision(0))
	if err != nil {
		t.Fatalf("LoadFromAll: %v", err)
	}
	events := collectAll(t, iter)
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	if events[0].StreamID != "stream-A" || events[1].StreamID != "stream-B" || events[2].StreamID != "stream-A" {
		t.Errorf("unexpected ordering: %v %v %v", events[0].StreamID, events[1].StreamID, events[2].StreamID)
	}
}

// TestLoadFromAll_IgnoresInFlightTransactions verifies that LoadFromAll never
// returns events beyond a gap caused by an uncommitted transaction.
//
// Scenario:
//   - TX1 (raw): inserts stream-A event (gets id=1), stays open
//   - TX2 (store.Save): inserts stream-B event (gets id=2), commits
//   - LoadFromAll must return nothing — id=2 is visible but id=1 is not yet
//     committed, so returning id=2 would skip id=1 permanently
//   - TX1 commits → LoadFromAll now returns both events in order
func TestLoadFromAll_IgnoresInFlightTransactions(t *testing.T) {
	pool := newPool(t)
	store := pgstore.NewEventStore(pool)
	ctx := context.Background()

	// Acquire a dedicated connection for the in-flight transaction so the pool
	// cannot reuse it for subsequent queries.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire conn: %v", err)
	}
	defer conn.Release()

	tx, err := conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}

	// Insert a raw event inside the open transaction (stream-A, id will be 1).
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

	// Commit a second event via the store (stream-B, id will be 2).
	_, err = store.Save(ctx, []cqrs.Envelope{newEnvelope("stream-B", &OrderCreated{OrderID: "B"})}, cqrs.Any{})
	if err != nil {
		tx.Rollback(ctx) //nolint:errcheck
		t.Fatalf("store.Save: %v", err)
	}

	// LoadFromAll must return zero events: id=2 is committed but id=1 is still
	// in-flight, so advancing past id=1 would cause a permanent skip.
	iter, err := store.LoadFromAll(ctx, cqrs.Revision(0))
	if err != nil {
		tx.Rollback(ctx) //nolint:errcheck
		t.Fatalf("LoadFromAll (gap open): %v", err)
	}
	events := collectAll(t, iter)
	if len(events) != 0 {
		t.Errorf("expected 0 events while TX1 is in-flight, got %d", len(events))
	}

	// Commit the in-flight transaction.
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit tx: %v", err)
	}

	// Now both events must be visible and correctly ordered.
	iter, err = store.LoadFromAll(ctx, cqrs.Revision(0))
	if err != nil {
		t.Fatalf("LoadFromAll (gap closed): %v", err)
	}
	events = collectAll(t, iter)
	if len(events) != 2 {
		t.Fatalf("expected 2 events after TX1 commits, got %d", len(events))
	}
	if events[0].StreamID != "stream-A" {
		t.Errorf("expected stream-A first, got %s", events[0].StreamID)
	}
	if events[1].StreamID != "stream-B" {
		t.Errorf("expected stream-B second, got %s", events[1].StreamID)
	}
}
