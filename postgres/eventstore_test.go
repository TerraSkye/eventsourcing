//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/eventsourcingtest"
	"github.com/terraskye/eventsourcing/postgres"
)

func collectAll(t *testing.T, iter *eventsourcing.Iterator[*eventsourcing.Envelope]) []*eventsourcing.Envelope {
	t.Helper()
	var results []*eventsourcing.Envelope
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
	store := postgres.NewEventStore(pool)

	ctx := context.Background()
	events := []eventsourcing.Envelope{
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1"}),
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1"}),
	}

	result, err := store.Save(ctx, events, eventsourcing.NoStream{})
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
	store := postgres.NewEventStore(pool)
	ctx := context.Background()

	_, err := store.Save(ctx, []eventsourcing.Envelope{
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1"}),
	}, eventsourcing.Any{})
	if err != nil {
		t.Fatalf("first save: %v", err)
	}

	_, err = store.Save(ctx, []eventsourcing.Envelope{
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1"}),
	}, eventsourcing.Revision(0)) // wrong revision
	if err == nil {
		t.Fatal("expected revision conflict error")
	}
	var conflictErr *eventsourcing.StreamRevisionConflictError
	if !errors.As(err, &conflictErr) {
		t.Errorf("expected StreamRevisionConflictError, got %T: %v", err, err)
	}
}

func TestSave_NoStream_FailsWhenStreamExists(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewEventStore(pool)
	ctx := context.Background()

	_, _ = store.Save(ctx, []eventsourcing.Envelope{newEnvelope("order-1", &OrderCreated{OrderID: "order-1"})}, eventsourcing.Any{})

	_, err := store.Save(ctx, []eventsourcing.Envelope{newEnvelope("order-1", &OrderCreated{OrderID: "order-1"})}, eventsourcing.NoStream{})
	if !errors.Is(err, eventsourcing.ErrStreamExists) {
		t.Errorf("expected ErrStreamExists, got %v", err)
	}
}

// slowEvent blocks in MarshalJSON until release is closed, signalling on
// entered when it starts.
type slowEvent struct {
	ID      string
	entered chan struct{}
	release chan struct{}
}

func (e *slowEvent) AggregateID() string { return e.ID }
func (e *slowEvent) EventType() string   { return "SlowEvent" }

func (e *slowEvent) MarshalJSON() ([]byte, error) {
	close(e.entered)
	<-e.release
	return json.Marshal(struct{ ID string }{e.ID})
}

// TestSave_MarshalsOutsideAppendLock covers that encoding a Save's events
// does not hold the append lock every Save shares: a Save whose payload is
// slow to marshal must not stall appends to other streams.
func TestSave_MarshalsOutsideAppendLock(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewEventStore(pool)
	ctx := context.Background()

	slow := &slowEvent{ID: "slow-1", entered: make(chan struct{}), release: make(chan struct{})}
	slowDone := make(chan error, 1)
	go func() {
		_, err := store.Save(ctx, []eventsourcing.Envelope{newEnvelope("slow-1", slow)}, eventsourcing.Any{})
		slowDone <- err
	}()
	defer func() {
		close(slow.release)
		if err := <-slowDone; err != nil {
			t.Errorf("slow save: %v", err)
		}
	}()

	select {
	case <-slow.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("slow save never started marshalling")
	}

	fastCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := store.Save(fastCtx, []eventsourcing.Envelope{newEnvelope("order-1", &OrderCreated{OrderID: "order-1"})}, eventsourcing.Any{}); err != nil {
		t.Fatalf("save to another stream while one is marshalling: %v", err)
	}
}

func TestLoadStream_NotFound(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewEventStore(pool)

	_, err := store.LoadStream(context.Background(), "no-such-stream")
	if !errors.Is(err, eventsourcing.ErrStreamNotFound) {
		t.Errorf("expected ErrStreamNotFound, got %v", err)
	}
}

func readAll(t *testing.T, reader eventsourcing.GlobalReader, after uint64) []*eventsourcing.Envelope {
	t.Helper()
	var out []*eventsourcing.Envelope
	for {
		res, err := reader.ReadAll(context.Background(), after, eventsourcing.ReadOptions{})
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
	store := postgres.NewEventStore(pool)
	ctx := context.Background()

	_, _ = store.Save(ctx, []eventsourcing.Envelope{newEnvelope("stream-A", &OrderCreated{OrderID: "A"})}, eventsourcing.Any{})
	_, _ = store.Save(ctx, []eventsourcing.Envelope{newEnvelope("stream-B", &OrderCreated{OrderID: "B"})}, eventsourcing.Any{})
	_, _ = store.Save(ctx, []eventsourcing.Envelope{newEnvelope("stream-A", &OrderCreated{OrderID: "A"})}, eventsourcing.Any{})

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
	store := postgres.NewEventStore(pool)
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
		_, err := store.Save(ctx, []eventsourcing.Envelope{newEnvelope("stream-B", &OrderCreated{OrderID: "B"})}, eventsourcing.Any{})
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
	if events[0].StreamID != "stream-A" || events[0].GlobalPosition != 1 ||
		events[1].StreamID != "stream-B" || events[1].GlobalPosition != 2 {
		t.Errorf("got %s@%d, %s@%d; want stream-A@1, stream-B@2",
			events[0].StreamID, events[0].GlobalPosition, events[1].StreamID, events[1].GlobalPosition)
	}
}

// TestReadAll_HugePositionIsRefusedNotReplayed: a position above the
// largest bigint cannot be bound as one; casting it would sign-flip to a
// negative id and replay the whole store, so ReadAll refuses it.
func TestReadAll_HugePositionIsRefusedNotReplayed(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewEventStore(pool)
	ctx := context.Background()

	if _, err := store.Save(ctx, []eventsourcing.Envelope{
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1"}),
	}, eventsourcing.NoStream{}); err != nil {
		t.Fatalf("save: %v", err)
	}

	res, err := store.ReadAll(ctx, math.MaxUint64, eventsourcing.ReadOptions{})
	if !errors.Is(err, eventsourcing.ErrInvalidRevision) {
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
	store := postgres.NewEventStore(pool)
	ctx := context.Background()

	_, err := store.Save(ctx, []eventsourcing.Envelope{
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1"}),
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1"}),
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1"}),
	}, eventsourcing.NoStream{})
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	defer assertRevisionOverflowPanic(t, "LoadStreamFrom(id, Revision(MaxUint64))")

	// The call should panic. If it doesn't, close the iterator so its open
	// rows don't hold the pool connection that pool.Close in cleanup waits
	// on, which would hang the run instead of failing it.
	iter, _ := store.LoadStreamFrom(ctx, "order-1", eventsourcing.Revision(math.MaxUint64))
	if iter != nil {
		iter.Close() //nolint:errcheck
	}
}

// assertRevisionOverflowPanic fails t unless the deferred call it guards
// panicked with eventsourcing.Revision's int64 overflow panic. call names the guarded
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
	eventsourcingtest.AcceptanceTest(t, postgres.NewEventStore(newPool(t)))
}

func TestGlobalReaderAcceptance(t *testing.T) {
	store := postgres.NewEventStore(newPool(t))
	eventsourcingtest.GlobalReaderAcceptanceTest(t, store, store)
}
