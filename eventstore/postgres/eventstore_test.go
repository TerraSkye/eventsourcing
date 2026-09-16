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
	"reflect"
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

// TestLoadFromAll_HugeRevisionReplaysEntireStoreInsteadOfNothing documents a
// bug: LoadFromAll's doc comment says a cqrs.Revision(n) position means
// "starting after the position identified by version" - so a caller that
// believes it has already consumed an enormous number of events (e.g. via a
// corrupted/bogus checkpoint, or the unsigned-underflow scenario the
// analogous eventstore/memory bug used to hit before it was fixed) should,
// at worst, get an empty iterator back, since no real global position can
// exceed such a value.
//
// Instead, LoadFromAll does:
//
//	fromID := version.ToRawInt64()  // int64(uint64) conversion
//	... WHERE id > $1 ...
//
// For any Revision >= 1<<63, ToRawInt64()'s int64(r) conversion produces a
// negative number. Since every real `id` in the events table is a positive
// bigserial, "id > <negative>" matches every row - so instead of returning
// nothing, LoadFromAll silently replays the entire store from the beginning.
// This is the LoadFromAll sibling of the already-filed
// eventstore-postgres-loadstreamfrom-huge-revision-replays-entire-stream bug (same root cause,
// same file, different function - that report only exercises LoadStreamFrom).
func TestLoadFromAll_HugeRevisionReplaysEntireStoreInsteadOfNothing(t *testing.T) {

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

	// A caller believes it has already processed math.MaxUint64 events (far
	// beyond the 3 actually stored globally) and asks to resume strictly
	// after that position - it should see nothing new.
	iter, err := store.LoadFromAll(ctx, cqrs.Revision(math.MaxUint64))
	if err != nil {
		t.Fatalf("LoadFromAll: %v", err)
	}
	events := collectAll(t, iter)

	if len(events) != 0 {
		t.Fatalf("documents bug: LoadFromAll(Revision(MaxUint64)) replayed %d event(s) from the "+
			"beginning of the store instead of returning none (int64(MaxUint64) == -1, so "+
			"'id > -1' matches every row)", len(events))
	}
}

// TestLoadStreamFrom_HugeRevisionReplaysEntireStreamInsteadOfNothing documents
// a bug: eventstore.LoadStreamFrom's doc comment says a cqrs.Revision(n) means
// "events after [position] n are returned" - so a caller who believes it has
// already processed an enormous number of events (e.g. via an unsigned
// underflow bug computing n, or simply a corrupted/bogus checkpoint) should,
// at worst, get an empty iterator back, since no real stream position can
// exceed such a value.
//
// Instead, LoadStreamFrom's default branch does:
//
//	fromPos := version.ToRawInt64()  // int64(uint64) conversion
//	... WHERE stream_position > $2 ...
//
// For any Revision >= 1<<63, ToRawInt64()'s int64(r) conversion produces a
// *negative* number. Since every real stream_position is positive, "stream_position
// > <negative>" matches every row in the stream - so instead of returning
// nothing (or erroring), LoadStreamFrom silently replays the *entire* stream
// from the beginning. This is the opposite of the documented "resume where
// you left off" semantics and, unlike the already-known sibling issue in
// eventstore/memory (which panics) and eventstore/file (which returns an
// empty stream), this manifests as silent full-stream reprocessing - the
// most dangerous of the three failure modes for a real caller (e.g. a
// projector replaying every event again from position 0).
func TestLoadStreamFrom_HugeRevisionReplaysEntireStreamInsteadOfNothing(t *testing.T) {

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

	// A caller believes it has already processed math.MaxUint64 events (a
	// value far beyond the 3 actually stored) and asks to resume strictly
	// after that position - it should see nothing new.
	iter, err := store.LoadStreamFrom(ctx, "order-1", cqrs.Revision(math.MaxUint64))
	if err != nil {
		t.Fatalf("LoadStreamFrom: %v", err)
	}
	events := collectAll(t, iter)

	if len(events) != 0 {
		t.Fatalf("documents bug: LoadStreamFrom(Revision(MaxUint64)) replayed %d event(s) from the "+
			"beginning of the stream instead of returning none (int64(MaxUint64) == -1, so "+
			"'stream_position > -1' matches every row)", len(events))
	}
}

// This file covers the serialize -> persist -> deserialize roundtrip for the
// postgres store: Save json.Marshal's the event into the payload BYTEA column
// and LoadStream rebuilds it from the registry. The shared registry/JSON half
// of that contract is pinned in the root package's serialization_test.go;
// here the bytes really go through postgres, which is also where the
// column types impose their own limits — notably occurred_at, a TIMESTAMPTZ,
// which only holds microseconds.

type money struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

type lineItem struct {
	SKU   string   `json:"sku"`
	Qty   int      `json:"qty"`
	Price money    `json:"price"`
	Notes []string `json:"notes"`
}

type shippingAddress struct {
	Street  string `json:"street"`
	Country string `json:"country"`
}

// roundtripEvent mixes nested structs, a slice of structs, set and nil
// pointers, maps, a uuid.UUID and a nanosecond-precision time.Time.
type roundtripEvent struct {
	OrderID  string            `json:"order_id"`
	TraceID  uuid.UUID         `json:"trace_id"`
	PlacedAt time.Time         `json:"placed_at"`
	Total    money             `json:"total"`
	Items    []lineItem        `json:"items"`
	ShipTo   *shippingAddress  `json:"ship_to"`
	BillTo   *shippingAddress  `json:"bill_to"`
	Labels   map[string]string `json:"labels"`
	Discount float64           `json:"discount"`
	Rushed   bool              `json:"rushed"`
}

func (e *roundtripEvent) AggregateID() string { return e.OrderID }
func (e *roundtripEvent) EventType() string   { return "roundtripEvent" }

func init() {
	cqrs.RegisterEvent(&roundtripEvent{})
}

func newRoundtripEvent(orderID string) *roundtripEvent {
	return &roundtripEvent{
		OrderID:  orderID,
		TraceID:  uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8"),
		PlacedAt: time.Date(2024, 3, 1, 12, 34, 56, 123456789, time.UTC),
		Total:    money{Amount: 4999, Currency: "EUR"},
		Items: []lineItem{
			{SKU: "WIDGET-1", Qty: 2, Price: money{Amount: 1999, Currency: "EUR"}, Notes: []string{"gift wrap"}},
			{SKU: "WIDGET-2", Qty: 1, Price: money{Amount: 1001, Currency: "EUR"}},
		},
		ShipTo:   &shippingAddress{Street: "Keizersgracht 1", Country: "NL"},
		BillTo:   nil,
		Labels:   map[string]string{"channel": "web"},
		Discount: 12.5,
		Rushed:   true,
	}
}

// TestSerializationRoundtrip_SaveAndLoadStream asserts that a rich event
// survives Save -> postgres -> LoadStream unchanged, including the
// nanosecond-precision time.Time carried *inside* the payload — that one goes
// through JSON, not through a timestamp column, so it keeps full precision.
func TestSerializationRoundtrip_SaveAndLoadStream(t *testing.T) {
	pool := newPool(t)
	store := pgstore.NewEventStore(pool)

	ctx := context.Background()
	want := newRoundtripEvent("order-1")
	eventID := uuid.New()
	occurredAt := time.Date(2024, 3, 1, 12, 34, 56, 987654321, time.UTC)

	env := cqrs.Envelope{
		EventID:    eventID,
		StreamID:   "order-1",
		Event:      want,
		OccurredAt: occurredAt,
		Metadata: map[string]any{
			"user":    "alice",
			"retries": 3,
			"trace":   map[string]any{"span": "abc"},
		},
	}

	if _, err := store.Save(ctx, []cqrs.Envelope{env}, cqrs.NoStream{}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	iter, err := store.LoadStream(ctx, "order-1")
	if err != nil {
		t.Fatalf("LoadStream: %v", err)
	}
	loaded := collectAll(t, iter)
	if len(loaded) != 1 {
		t.Fatalf("LoadStream returned %d events, want 1", len(loaded))
	}
	got := loaded[0]

	gotEvent, ok := got.Event.(*roundtripEvent)
	if !ok {
		t.Fatalf("loaded event is %T, want *roundtripEvent", got.Event)
	}
	if !reflect.DeepEqual(gotEvent, want) {
		t.Fatalf("event changed on the way through postgres:\n got: %#v\nwant: %#v", gotEvent, want)
	}

	// The pieces DeepEqual would also pass on if the whole substructure went
	// missing, called out so a regression names itself.
	if gotEvent.TraceID != want.TraceID {
		t.Errorf("TraceID = %v, want %v", gotEvent.TraceID, want.TraceID)
	}
	if !gotEvent.PlacedAt.Equal(want.PlacedAt) || gotEvent.PlacedAt.Nanosecond() != 123456789 {
		t.Errorf("PlacedAt = %v, want %v with nanoseconds intact", gotEvent.PlacedAt, want.PlacedAt)
	}
	if gotEvent.BillTo != nil {
		t.Errorf("BillTo = %#v, want a nil pointer to survive as nil", gotEvent.BillTo)
	}
	if gotEvent.Items[1].Notes != nil {
		t.Errorf("Items[1].Notes = %#v, want a nil slice to survive as nil", gotEvent.Items[1].Notes)
	}

	if got.EventID != eventID {
		t.Errorf("EventID = %v, want %v", got.EventID, eventID)
	}
	if got.StreamID != "order-1" {
		t.Errorf("StreamID = %q, want %q", got.StreamID, "order-1")
	}

	// Metadata is a map[string]any, so JSON — not the caller — decides the Go
	// type of every value: numbers always come back as float64.
	if got.Metadata["user"] != "alice" {
		t.Errorf(`Metadata["user"] = %#v, want "alice"`, got.Metadata["user"])
	}
	if v, ok := got.Metadata["retries"].(float64); !ok || v != 3 {
		t.Errorf(`Metadata["retries"] = %#v (%[1]T), want float64(3)`, got.Metadata["retries"])
	}
	trace, ok := got.Metadata["trace"].(map[string]any)
	if !ok {
		t.Fatalf(`Metadata["trace"] = %#v (%[1]T), want map[string]any`, got.Metadata["trace"])
	}
	if trace["span"] != "abc" {
		t.Errorf(`Metadata["trace"]["span"] = %#v, want "abc"`, trace["span"])
	}
}

// TestSerializationRoundtrip_OccurredAtLosesNanoseconds pins a genuine
// fidelity limit of this backend: Envelope.OccurredAt is stored in a
// TIMESTAMPTZ column, which holds microseconds, so the sub-microsecond part
// of the timestamp does not come back. Anything needing nanosecond precision
// has to live in the event payload, which is JSON and keeps it.
func TestSerializationRoundtrip_OccurredAtLosesNanoseconds(t *testing.T) {
	pool := newPool(t)
	store := pgstore.NewEventStore(pool)

	ctx := context.Background()
	occurredAt := time.Date(2024, 3, 1, 12, 34, 56, 987654321, time.UTC)

	if _, err := store.Save(ctx, []cqrs.Envelope{{
		EventID:    uuid.New(),
		StreamID:   "order-2",
		Event:      newRoundtripEvent("order-2"),
		OccurredAt: occurredAt,
	}}, cqrs.NoStream{}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	iter, err := store.LoadStream(ctx, "order-2")
	if err != nil {
		t.Fatalf("LoadStream: %v", err)
	}
	loaded := collectAll(t, iter)
	if len(loaded) != 1 {
		t.Fatalf("LoadStream returned %d events, want 1", len(loaded))
	}

	wantTruncated := occurredAt.Truncate(time.Microsecond)
	if !loaded[0].OccurredAt.Equal(wantTruncated) {
		t.Fatalf("OccurredAt = %v, want %v (the microsecond truncation TIMESTAMPTZ imposes)",
			loaded[0].OccurredAt.UTC(), wantTruncated)
	}
	if loaded[0].OccurredAt.Equal(occurredAt) {
		t.Fatal("OccurredAt kept its nanoseconds; if the column type changed, this expectation can be tightened")
	}

	// The nanosecond-precision timestamp inside the JSON payload is unaffected.
	if got := loaded[0].Event.(*roundtripEvent).PlacedAt; got.Nanosecond() != 123456789 {
		t.Errorf("payload PlacedAt = %v, want nanoseconds intact", got)
	}
}

// TestSerializationRoundtrip_BatchKeepsEventsDistinct asserts that each event
// in a multi-event batch is decoded into its own instance: a decode that
// reused one target would leave every loaded event holding the last one's
// payload.
func TestSerializationRoundtrip_BatchKeepsEventsDistinct(t *testing.T) {
	pool := newPool(t)
	store := pgstore.NewEventStore(pool)

	ctx := context.Background()
	var envs []cqrs.Envelope
	for i := range 3 {
		ev := newRoundtripEvent("order-3")
		ev.Total = money{Amount: int64(100 * (i + 1)), Currency: "EUR"}
		ev.Labels = map[string]string{"seq": string(rune('a' + i))}
		envs = append(envs, cqrs.Envelope{
			EventID:    uuid.New(),
			StreamID:   "order-3",
			Event:      ev,
			OccurredAt: time.Date(2024, 3, 1, 12, 0, i, 0, time.UTC),
			Metadata:   map[string]any{"seq": i},
		})
	}

	if _, err := store.Save(ctx, envs, cqrs.NoStream{}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	iter, err := store.LoadStream(ctx, "order-3")
	if err != nil {
		t.Fatalf("LoadStream: %v", err)
	}
	loaded := collectAll(t, iter)
	if len(loaded) != len(envs) {
		t.Fatalf("LoadStream returned %d events, want %d", len(loaded), len(envs))
	}

	for i, got := range loaded {
		want := envs[i].Event.(*roundtripEvent)
		gotEvent, ok := got.Event.(*roundtripEvent)
		if !ok {
			t.Fatalf("loaded[%d] is %T, want *roundtripEvent", i, got.Event)
		}
		if !reflect.DeepEqual(gotEvent, want) {
			t.Errorf("loaded[%d] = %#v, want %#v", i, gotEvent, want)
		}
		if got.EventID != envs[i].EventID {
			t.Errorf("loaded[%d].EventID = %v, want %v", i, got.EventID, envs[i].EventID)
		}
		if got.Version != uint64(i+1) {
			t.Errorf("loaded[%d].Version = %d, want %d", i, got.Version, i+1)
		}
		if v, ok := got.Metadata["seq"].(float64); !ok || int(v) != i {
			t.Errorf(`loaded[%d].Metadata["seq"] = %#v, want float64(%d)`, i, got.Metadata["seq"], i)
		}
	}
}

// TestSerializationRoundtrip_UnknownFieldInStoredPayload asserts that an event
// written by an older build — carrying a field the current struct no longer
// has — still loads, so an additive schema change does not break replay of
// streams that are already persisted.
func TestSerializationRoundtrip_UnknownFieldInStoredPayload(t *testing.T) {
	pool := newPool(t)
	store := pgstore.NewEventStore(pool)

	ctx := context.Background()
	if _, err := store.Save(ctx, []cqrs.Envelope{{
		EventID:  uuid.New(),
		StreamID: "order-4",
		Event:    newRoundtripEvent("order-4"),
	}}, cqrs.NoStream{}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Rewrite the stored payload the way an older build would have left it:
	// an extra field that no longer exists on roundtripEvent.
	legacy := []byte(`{"order_id":"order-4","discount":7.5,"legacy_reason":"promo","total":{"amount":10,"currency":"EUR","vat":21}}`)
	if _, err := pool.Exec(ctx, `UPDATE events SET payload = $1 WHERE stream_id = $2`, legacy, "order-4"); err != nil {
		t.Fatalf("rewrite payload: %v", err)
	}

	iter, err := store.LoadStream(ctx, "order-4")
	if err != nil {
		t.Fatalf("LoadStream: %v", err)
	}
	loaded := collectAll(t, iter)
	if len(loaded) != 1 {
		t.Fatalf("LoadStream returned %d events, want 1", len(loaded))
	}

	got, ok := loaded[0].Event.(*roundtripEvent)
	if !ok {
		t.Fatalf("loaded event is %T, want *roundtripEvent", loaded[0].Event)
	}
	if got.OrderID != "order-4" {
		t.Errorf("OrderID = %q, want %q", got.OrderID, "order-4")
	}
	if got.Discount != 7.5 {
		t.Errorf("Discount = %v, want 7.5", got.Discount)
	}
	if (got.Total != money{Amount: 10, Currency: "EUR"}) {
		t.Errorf("Total = %#v, want money{10, EUR}", got.Total)
	}
	// Fields absent from the older payload stay at their zero value.
	if got.Items != nil {
		t.Errorf("Items = %#v, want nil for a field the stored payload never had", got.Items)
	}
	if !got.PlacedAt.IsZero() {
		t.Errorf("PlacedAt = %v, want the zero time", got.PlacedAt)
	}
}

// TestSerializationRoundtrip_PayloadBytesAreExactlyWhatWasMarshalled asserts
// the payload column is a byte-for-byte copy of json.Marshal's output. It is
// a BYTEA rather than a jsonb, so postgres stores it verbatim and does not
// reorder keys, drop duplicates or renormalise numbers on the way in.
func TestSerializationRoundtrip_PayloadBytesAreExactlyWhatWasMarshalled(t *testing.T) {
	pool := newPool(t)
	store := pgstore.NewEventStore(pool)

	ctx := context.Background()
	event := newRoundtripEvent("order-5")

	if _, err := store.Save(ctx, []cqrs.Envelope{{
		EventID:  uuid.New(),
		StreamID: "order-5",
		Event:    event,
	}}, cqrs.NoStream{}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var payload []byte
	if err := pool.QueryRow(ctx, `SELECT payload FROM events WHERE stream_id = $1`, "order-5").Scan(&payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}

	want, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(payload) != string(want) {
		t.Fatalf("stored payload = %s\nwant                  %s", payload, want)
	}
}
