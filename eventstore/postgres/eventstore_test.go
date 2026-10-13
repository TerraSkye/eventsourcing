//go:build integration

package postgres_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	cqrs "github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/eventsourcingtest"
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
	var results []*cqrs.Envelope
	for iter.Next() {
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

func readAll(t *testing.T, reader cqrs.GlobalReader, after uint64) []*cqrs.Envelope {
	t.Helper()
	var out []*cqrs.Envelope
	for {
		res, err := reader.ReadAll(context.Background(), after, cqrs.ReadOptions{})
		if err != nil {
			t.Fatalf("ReadAll(after %d): %v", after, err)
		}
		out = append(out, res.Events...)
		after = res.Next
		if res.AtEnd {
			return out
		}
	}
}

func TestReadAll_BasicOrdering(t *testing.T) {
	pool := newPool(t)
	store := pgstore.NewEventStore(pool)
	ctx := context.Background()

	_, _ = store.Save(ctx, []cqrs.Envelope{newEnvelope("stream-A", &OrderCreated{OrderID: "A"})}, cqrs.Any{})
	_, _ = store.Save(ctx, []cqrs.Envelope{newEnvelope("stream-B", &OrderCreated{OrderID: "B"})}, cqrs.Any{})
	_, _ = store.Save(ctx, []cqrs.Envelope{newEnvelope("stream-A", &OrderCreated{OrderID: "A"})}, cqrs.Any{})

	events := readAll(t, store, 0)
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	if events[0].StreamID != "stream-A" || events[1].StreamID != "stream-B" || events[2].StreamID != "stream-A" {
		t.Errorf("unexpected ordering: %v %v %v", events[0].StreamID, events[1].StreamID, events[2].StreamID)
	}
}

// Save's append lock; see appendLockKey1 and appendLockKey2 in the store.
const appendLockKey1, appendLockKey2 = 0x65766e74, 0x616c6c00

// TestReadAll_AppendsBecomeVisibleInIDOrder pins the guarantee ReadAll's
// gap-freedom rests on: an append that has taken ids but not committed holds
// back every later append, so no id can become visible before a lower one.
//
// Before, appends ran in parallel and ReadAll filtered on the oldest
// in-flight transaction (xmin). That missed events: a multi-event append A
// inserting ids 50 and 52 around another append B's id 51 commits first;
// B's transaction ID is higher than A's, so once A commits, xmin moves past
// A, ReadAll returns 50 and 52, the reader checkpoints 52, and 51 is never
// read when B commits.
//
// Scenario:
//   - TX1 (raw, following Save's protocol): takes the append lock and
//     inserts stream-A's event (id 1), stays open
//   - store.Save to stream-B starts and must wait for the append lock
//   - ReadAll returns nothing: id 1 is not committed, and nothing else may
//     be visible yet
//   - TX1 commits; the Save then gets id 2 and commits; ReadAll returns 1, 2
func TestReadAll_AppendsBecomeVisibleInIDOrder(t *testing.T) {
	pool := newPool(t)
	store := pgstore.NewEventStore(pool)
	ctx := context.Background()

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire conn: %v", err)
	}
	defer conn.Release()

	tx, err := conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1, $2)", appendLockKey1, appendLockKey2); err != nil {
		t.Fatalf("take append lock: %v", err)
	}
	payload, _ := json.Marshal(&OrderCreated{OrderID: "A"})
	if _, err := tx.Exec(ctx, `
		INSERT INTO events (event_id, stream_id, stream_position, event_type, payload, metadata, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		uuid.New(), "stream-A", 1, "OrderCreated", payload, []byte("{}"), time.Now(),
	); err != nil {
		t.Fatalf("raw insert: %v", err)
	}

	saved := make(chan error, 1)
	go func() {
		_, err := store.Save(ctx, []cqrs.Envelope{newEnvelope("stream-B", &OrderCreated{OrderID: "B"})}, cqrs.Any{})
		saved <- err
	}()

	select {
	case err := <-saved:
		t.Fatalf("Save completed (err = %v) while another append held the append lock; appends can commit out of id order", err)
	case <-time.After(300 * time.Millisecond):
	}
	if events := readAll(t, store, 0); len(events) != 0 {
		t.Errorf("ReadAll returned %d events while id 1 is uncommitted, want 0", len(events))
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit tx: %v", err)
	}
	if err := <-saved; err != nil {
		t.Fatalf("Save: %v", err)
	}

	events := readAll(t, store, 0)
	if len(events) != 2 {
		t.Fatalf("expected 2 events after both commit, got %d", len(events))
	}
	if events[0].StreamID != "stream-A" || events[0].GlobalVersion != 1 ||
		events[1].StreamID != "stream-B" || events[1].GlobalVersion != 2 {
		t.Errorf("got %s@%d, %s@%d; want stream-A@1, stream-B@2",
			events[0].StreamID, events[0].GlobalVersion, events[1].StreamID, events[1].GlobalVersion)
	}
}

// TestReadAll_HugePositionIsRefusedNotReplayed: a position above the
// largest bigint cannot be bound as one; casting it would sign-flip to a
// negative id and replay the whole store, so ReadAll refuses it.
func TestReadAll_HugePositionIsRefusedNotReplayed(t *testing.T) {
	pool := newPool(t)
	store := pgstore.NewEventStore(pool)
	ctx := context.Background()

	if _, err := store.Save(ctx, []cqrs.Envelope{
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1"}),
	}, cqrs.NoStream{}); err != nil {
		t.Fatalf("save: %v", err)
	}

	res, err := store.ReadAll(ctx, math.MaxUint64, cqrs.ReadOptions{})
	if !errors.Is(err, cqrs.ErrInvalidRevision) {
		t.Fatalf("ReadAll(after MaxUint64) = %d events, err %v; want ErrInvalidRevision", len(res.Events), err)
	}
}

// TestLoadStreamFrom_HugeRevisionIsRefusedNotReplayed covers what an
// unrepresentable Revision does to LoadStreamFrom.
//
// Revision is a uint64 and ToRawInt64 returns int64, so any Revision >= 1<<63
// used to sign-flip negative. Here that turned `stream_position > $2` into
// `stream_position > -1` and replayed the entire stream. ToRawInt64 now
// panics rather than returning a value that means something else.
func TestLoadStreamFrom_HugeRevisionIsRefusedNotReplayed(t *testing.T) {

	pool := newPool(t)
	store := pgstore.NewEventStore(pool)
	ctx := context.Background()

	_, err := store.Save(ctx, []cqrs.Envelope{
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1"}),
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1"}),
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1"}),
	}, cqrs.NoStream{})
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	defer assertRevisionOverflowPanic(t, "LoadStreamFrom(id, Revision(MaxUint64))")

	// The call should panic. If it doesn't, close the iterator so its open
	// rows don't hold the pool connection that pool.Close in cleanup waits
	// on, which would hang the run instead of failing it.
	iter, _ := store.LoadStreamFrom(ctx, "order-1", cqrs.Revision(math.MaxUint64))
	if iter != nil {
		iter.Close() //nolint:errcheck
	}
}

// assertRevisionOverflowPanic fails t unless the deferred call it guards
// panicked with cqrs.Revision's int64 overflow panic. call names the guarded
// call for the failure message.
func assertRevisionOverflowPanic(t *testing.T, call string) {
	t.Helper()
	r := recover()
	if r == nil {
		t.Fatalf("%s did not panic: an unrepresentable revision is being read "+
			"as a marker value again, which replays events from the beginning", call)
	}
	if msg, ok := r.(string); !ok || !strings.Contains(msg, "overflows int64") {
		t.Fatalf("%s panicked with %v, want the Revision overflow panic", call, r)
	}
}

func TestAcceptance(t *testing.T) {
	eventsourcingtest.AcceptanceTest(t, pgstore.NewEventStore(newPool(t)))
}

func TestGlobalReaderAcceptance(t *testing.T) {
	store := pgstore.NewEventStore(newPool(t))
	eventsourcingtest.GlobalReaderAcceptanceTest(t, store, store)
}
