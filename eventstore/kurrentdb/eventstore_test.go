package kurrentdb_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	cqrs "github.com/terraskye/eventsourcing"
	kdbstore "github.com/terraskye/eventsourcing/eventstore/kurrentdb"

	"github.com/cenkalti/backoff/v4"
	"github.com/kurrent-io/KurrentDB-Client-Go/kurrentdb"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

type CheckEvent struct {
	N int
}

func (CheckEvent) AggregateID() string { return "check" }
func (CheckEvent) EventType() string   { return "CheckEvent" }

var testDB *kurrentdb.Client

// TestMain starts a single kurrentdb container for every test in this
// package, so each test doesn't pay its own container-startup cost.
func TestMain(m *testing.M) {
	cqrs.RegisterEventByType(func() cqrs.Event { return &CheckEvent{} })

	ctx := context.Background()
	req := testcontainers.ContainerRequest{
		Image:        "kurrentplatform/kurrentdb:latest",
		ExposedPorts: []string{"2113/tcp"},
		Env: map[string]string{
			"EVENTSTORE_INSECURE":        "true",
			"EVENTSTORE_RUN_PROJECTIONS": "None",
			"EVENTSTORE_MEM_DB":          "true",
			"EVENTSTORE_CLUSTER_SIZE":    "1",
		},
		WaitingFor: wait.ForLog("IS LEADER... SPARTA!").WithStartupTimeout(60 * time.Second),
	}
	kc, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		log.Fatalf("start kurrentdb container: %v", err)
	}
	defer kc.Terminate(ctx) //nolint:errcheck

	host, err := kc.Host(ctx)
	if err != nil {
		log.Fatalf("get host: %v", err)
	}
	port, err := kc.MappedPort(ctx, "2113")
	if err != nil {
		log.Fatalf("get port: %v", err)
	}

	settings, err := kurrentdb.ParseConnectionString(fmt.Sprintf("esdb://%s:%s?tls=false", host, port.Port()))
	if err != nil {
		log.Fatalf("parse connection string: %v", err)
	}
	testDB, err = kurrentdb.NewClient(settings)
	if err != nil {
		log.Fatalf("new client: %v", err)
	}

	os.Exit(m.Run())
}

func collectAll(t *testing.T, iter *cqrs.Iterator[*cqrs.Envelope]) []*cqrs.Envelope {
	t.Helper()
	var out []*cqrs.Envelope
	for iter.Next(context.Background()) {
		out = append(out, iter.Value())
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	return out
}

// TestLoadStreamFrom_RevisionIsInclusiveNotExclusive is a regression test
// for GitHub issue #45: LoadStreamFrom(ctx, id, cqrs.Revision(N)) read
// starting at revision N inclusive, because it forwarded N directly into
// kurrentdb.StreamRevision{Value: N} — KurrentDB's own native,
// inclusive-of-N read position. Every other EventStore implementation in
// this repo treats Revision(N) as "N events already consumed, resume
// strictly after N" (exclusive), which is exactly the contract
// NewCommandHandler's retry loop depends on: resuming a load from a
// previously-seen revision re-delivered the last-consumed event a second
// time, corrupting aggregate state by evolving it twice.
func TestLoadStreamFrom_RevisionIsInclusiveNotExclusive(t *testing.T) {
	store := kdbstore.NewEventStore(testDB)
	ctx := context.Background()
	streamID := "check-inclusive"

	// Save 3 events (native KurrentDB revisions 0, 1, 2).
	for i := 0; i < 3; i++ {
		_, err := store.Save(ctx, []cqrs.Envelope{{
			StreamID:   streamID,
			Event:      &CheckEvent{N: i},
			Metadata:   map[string]any{},
			OccurredAt: time.Now(),
		}}, cqrs.Any{})
		if err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}

	// Simulate NewCommandHandler's evolve loop: consume everything once and
	// remember the last event's Version, exactly as command_handler.go does
	// via `revision = Revision(event.Version)`.
	iter, err := store.LoadStreamFrom(ctx, streamID, cqrs.Any{})
	if err != nil {
		t.Fatalf("initial load: %v", err)
	}
	all := collectAll(t, iter)
	if len(all) != 3 {
		t.Fatalf("expected 3 events, got %d", len(all))
	}
	lastVersion := all[len(all)-1].Version // native EventNumber = 2

	// Retry: resume from the last consumed revision. Since all 3 events
	// (indices 0,1,2) were already evolved, this MUST yield zero events.
	iter, err = store.LoadStreamFrom(ctx, streamID, cqrs.Revision(lastVersion))
	if err != nil {
		t.Fatalf("resume load: %v", err)
	}
	resumed := collectAll(t, iter)
	if len(resumed) != 0 {
		t.Errorf("expected 0 events when resuming from the last consumed revision %d, got %d: re-delivered event(s) %v",
			lastVersion, len(resumed), resumed)
	}
}

// TestLoadFromAll_HangsPastLastEvent is a regression test for GitHub issue
// #44: LoadFromAll passed count=0 to (*kurrentdb.Client).ReadAll, unlike
// LoadStream/LoadStreamFrom in the same file, which both pass 5000. Against
// a real server, count=0 does not bound the read — once the iterator
// consumed every currently-existing event, Next blocked forever instead of
// returning false.
func TestLoadFromAll_HangsPastLastEvent(t *testing.T) {
	store := kdbstore.NewEventStore(testDB)
	ctx := context.Background()
	streamID := "check-loadfromall-hangs"

	// Save 3 events so the $all stream has a known non-empty tail.
	for i := 0; i < 3; i++ {
		if _, err := store.Save(ctx, []cqrs.Envelope{{
			StreamID:   streamID,
			Event:      &CheckEvent{N: i},
			Metadata:   map[string]any{},
			OccurredAt: time.Now(),
		}}, cqrs.Any{}); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}

	iter, err := store.LoadFromAll(ctx, cqrs.Any{})
	if err != nil {
		t.Fatalf("load from all: %v", err)
	}

	// Drain every event currently in the $all stream. Once exhausted, Next
	// MUST return false (with iter.Err() == nil) rather than blocking forever.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for iter.Next(ctx) {
		}
	}()

	select {
	case <-done:
		// The point of this test is that the drain loop terminates at all
		// (the bug was that it never did). $all also carries KurrentDB's own
		// internal events (e.g. "$metadata"), which this store's registry
		// was never meant to decode, so a non-EOF error here is expected and
		// not itself a failure — only a timeout is.
		if err := iter.Err(); err != nil && !errors.Is(err, io.EOF) {
			t.Logf("iterator ended with a non-EOF error (expected for unregistered system events in $all): %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("LoadFromAll's iterator did not terminate within 15s after exhausting the $all stream: " +
			"Next() blocks forever instead of returning false, because LoadFromAll passes count=0 to " +
			"(*kurrentdb.Client).ReadAll (eventstore.go)")
	}
}

type BackoffProbeEvent struct{ N int }

func (BackoffProbeEvent) AggregateID() string { return "backoff-probe" }
func (BackoffProbeEvent) EventType() string   { return "BackoffProbeEvent" }

// TestSave_UsesConfiguredBackoffNotDefault verifies Save actually retries
// with the [kdbstore.Option] backoff it's given via [kdbstore.WithBackoff],
// rather than silently falling back to a fresh, unconfigured
// backoff.NewExponentialBackOff() (default MaxElapsedTime 15m). It uses a
// short test-only backoff instead of Save's real 30s default so the test
// runs in well under a second while still exercising the same code path.
func TestSave_UsesConfiguredBackoffNotDefault(t *testing.T) {
	const maxElapsed = 200 * time.Millisecond

	store := kdbstore.NewEventStore(testDB, kdbstore.WithBackoff(func() backoff.BackOff {
		b := backoff.NewExponentialBackOff()
		b.MaxElapsedTime = maxElapsed
		b.InitialInterval = 10 * time.Millisecond
		b.MaxInterval = 50 * time.Millisecond
		return b
	}))

	// A context that has already expired before Save's first attempt makes
	// every retry attempt fail the same way: the KurrentDB client reports a
	// retryable codes.DeadlineExceeded for each call, so the only thing
	// bounding Save's total run time is the backoff it uses between
	// attempts -- exactly the configuration this test verifies is honored.
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	time.Sleep(5 * time.Millisecond)

	start := time.Now()
	_, err := store.Save(ctx, []cqrs.Envelope{{
		StreamID: "backoff-probe-stream",
		Event:    BackoffProbeEvent{N: 1},
		Metadata: map[string]any{},
	}}, cqrs.Any{})
	elapsed := time.Since(start)

	t.Logf("Save gave up after %s, err=%v", elapsed, err)

	// Generous margin over maxElapsed for in-flight call overhead. If Save
	// ignored the configured backoff and fell back to an unconfigured
	// default (MaxElapsedTime 15m), elapsed would blow past this ceiling by
	// orders of magnitude, not by a small margin.
	const ceiling = 2 * time.Second
	if elapsed > ceiling {
		t.Errorf("Save took %s to give up, want <= %s (configured via WithBackoff with "+
			"MaxElapsedTime=%s; Save appears to be using a different backoff instead)",
			elapsed, ceiling, maxElapsed)
	}
}

type CountProbeEvent struct{ N int }

func (CountProbeEvent) AggregateID() string { return "count-probe" }
func (CountProbeEvent) EventType() string   { return "CountProbeEvent" }

func init() {
	cqrs.RegisterEventByType(func() cqrs.Event { return &CountProbeEvent{} })
}

// TestLoadStream_TruncatesStreamsLargerThanHardcodedCount documents a bug:
// LoadStream's doc comment promises "a lazy iterator over all events in the
// stream identified by id", but eventstore.go passes a hardcoded literal
// 5000 as the `count` argument to (*kurrentdb.Client).ReadStream. That count
// is not a page/batch size the client transparently re-requests past --
// KurrentDB's ReadReq.Options.CountOption bounds the entire single-request
// read server-side (confirmed by reading the vendor client's
// toReadStreamRequest/readInternal/ReadStream.Recv: once `count` events are
// delivered the server ends the gRPC stream, which Recv reports as a plain
// io.EOF, identical to a real end-of-stream). So a stream with more than
// 5000 events silently yields only its first 5000 to the caller, with
// iter.Err() == nil -- indistinguishable from a normal, complete read.
//
// This test is intentionally slow (appends 5001 events, then reads them
// back) and is skipped by default; un-skip it to reproduce.
func TestLoadStream_TruncatesStreamsLargerThanHardcodedCount(t *testing.T) {

	store := kdbstore.NewEventStore(testDB)
	ctx := context.Background()
	streamID := "count-probe-stream"

	const total = 5001
	const batchSize = 500

	for start := 0; start < total; start += batchSize {
		end := start + batchSize
		if end > total {
			end = total
		}
		batch := make([]cqrs.Envelope, 0, end-start)
		for i := start; i < end; i++ {
			batch = append(batch, cqrs.Envelope{
				StreamID:   streamID,
				Event:      &CountProbeEvent{N: i},
				Metadata:   map[string]any{},
				OccurredAt: time.Now(),
			})
		}
		if _, err := store.Save(ctx, batch, cqrs.Any{}); err != nil {
			t.Fatalf("save batch [%d,%d): %v", start, end, err)
		}
	}

	iter, err := store.LoadStream(ctx, streamID)
	if err != nil {
		t.Fatalf("load stream: %v", err)
	}

	count := 0
	for iter.Next(ctx) {
		count++
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}

	if count != total {
		t.Errorf("LoadStream returned %d events for a %d-event stream, want %d "+
			"(eventstore.go's LoadStream passes a hardcoded count=5000 to "+
			"(*kurrentdb.Client).ReadStream, silently truncating any stream larger "+
			"than that instead of returning every event as its doc comment promises)",
			count, total, total)
	}
}

// TestLoadStreamFrom_HugeRevisionReplaysEntireStreamInsteadOfNothing is a
// regression test documenting the bug filed as
// .bug/eventstore-kurrentdb-loadstreamfrom-huge-revision-replays-entire-stream.md.
//
// eventstore/kurrentdb/eventstore.go's LoadStreamFrom decides whether to
// honor the requested start position with `if version.ToRawInt64() > 0`.
// revision.go's Revision.ToRawInt64() is `int64(r)`, which silently sign-flips
// negative for any Revision >= 1<<63 (the same root cause already filed
// against eventstore/memory and eventstore/postgres). Here that makes the
// `> 0` check false, so LoadStreamFrom falls through to `kurrentdb.Start{}`
// — the very beginning of the stream — instead of erroring or returning
// nothing for a revision far beyond the stream's actual length. A caller
// resuming from what it believes is a huge, already-consumed revision
// instead gets the entire stream replayed from scratch.
func TestLoadStreamFrom_HugeRevisionReplaysEntireStreamInsteadOfNothing(t *testing.T) {

	store := kdbstore.NewEventStore(testDB)
	ctx := context.Background()
	streamID := "check-huge-revision-kurrentdb"

	// Save 3 events (native KurrentDB revisions 0, 1, 2).
	for i := 0; i < 3; i++ {
		_, err := store.Save(ctx, []cqrs.Envelope{{
			StreamID:   streamID,
			Event:      &CheckEvent{N: i},
			Metadata:   map[string]any{},
			OccurredAt: time.Now(),
		}}, cqrs.Any{})
		if err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}

	// A huge Revision, far beyond the stream's 3 events, sign-flips negative
	// through ToRawInt64()'s int64(r) conversion.
	hugeRevision := cqrs.Revision(uint64(1) << 63)

	iter, err := store.LoadStreamFrom(ctx, streamID, hugeRevision)
	if err != nil {
		t.Fatalf("load stream from huge revision: %v", err)
	}

	got := collectAll(t, iter)
	if len(got) != 0 {
		t.Errorf("LoadStreamFrom(id, Revision(1<<63)) replayed the entire stream from the beginning "+
			"(got %d events) instead of returning 0 events for a revision far beyond the stream's length: "+
			"ToRawInt64() sign-flipped negative, so the `version.ToRawInt64() > 0` check in LoadStreamFrom "+
			"fell through to kurrentdb.Start{}", len(got))
	}
}

// This file covers the serialize -> persist -> deserialize roundtrip for the
// kurrentdb store: Save json.Marshal's the event into the KurrentDB event's
// Data (and the metadata into UserMetadata), and LoadStream rebuilds it from
// the registry. The shared registry/JSON half of that contract is pinned in
// the root package's serialization_test.go; here the bytes really go through
// KurrentDB.

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
// survives Save -> KurrentDB -> LoadStream unchanged, along with the EventID
// and Metadata around it.
//
// Envelope.OccurredAt is deliberately not asserted: this backend does not
// persist the caller's value at all, it reports the server's CreatedDate on
// the way out (see eventstore.go, where OccurredAt is set from
// kEvent.Event.CreatedDate). A timestamp that must survive belongs in the
// event payload, which is JSON and keeps it to the nanosecond.
func TestSerializationRoundtrip_SaveAndLoadStream(t *testing.T) {
	store := kdbstore.NewEventStore(testDB)
	ctx := context.Background()
	streamID := "roundtrip-single"

	want := newRoundtripEvent("order-1")
	eventID := uuid.New()

	if _, err := store.Save(ctx, []cqrs.Envelope{{
		EventID:    eventID,
		StreamID:   streamID,
		Event:      want,
		OccurredAt: time.Now(),
		Metadata: map[string]any{
			"user":    "alice",
			"retries": 3,
			"trace":   map[string]any{"span": "abc"},
		},
	}}, cqrs.NoStream{}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	iter, err := store.LoadStream(ctx, streamID)
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
		t.Fatalf("event changed on the way through kurrentdb:\n got: %#v\nwant: %#v", gotEvent, want)
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
	if got.StreamID != streamID {
		t.Errorf("StreamID = %q, want %q", got.StreamID, streamID)
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

// TestSerializationRoundtrip_BatchKeepsEventsDistinct asserts that each event
// in a multi-event batch is decoded into its own instance: a decode that
// reused one target would leave every loaded event holding the last one's
// payload.
func TestSerializationRoundtrip_BatchKeepsEventsDistinct(t *testing.T) {
	store := kdbstore.NewEventStore(testDB)
	ctx := context.Background()
	streamID := "roundtrip-batch"

	var envs []cqrs.Envelope
	for i := range 3 {
		ev := newRoundtripEvent("order-2")
		ev.Total = money{Amount: int64(100 * (i + 1)), Currency: "EUR"}
		ev.Labels = map[string]string{"seq": string(rune('a' + i))}
		envs = append(envs, cqrs.Envelope{
			EventID:    uuid.New(),
			StreamID:   streamID,
			Event:      ev,
			OccurredAt: time.Now(),
			Metadata:   map[string]any{"seq": i},
		})
	}

	if _, err := store.Save(ctx, envs, cqrs.NoStream{}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	iter, err := store.LoadStream(ctx, streamID)
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
		if v, ok := got.Metadata["seq"].(float64); !ok || int(v) != i {
			t.Errorf(`loaded[%d].Metadata["seq"] = %#v, want float64(%d)`, i, got.Metadata["seq"], i)
		}
	}
}

// TestSerializationRoundtrip_UnknownFieldInStoredPayload asserts that an event
// written by an older build — carrying a field the current struct no longer
// has — still loads, so an additive schema change does not break replay of
// streams that are already persisted. The stored payload is produced by
// appending raw JSON under the same event type name, which is exactly what an
// older build of the application would have written.
func TestSerializationRoundtrip_UnknownFieldInStoredPayload(t *testing.T) {
	store := kdbstore.NewEventStore(testDB)
	ctx := context.Background()
	streamID := "roundtrip-unknown-fields"

	legacy := legacyPayloadEvent{
		OrderID:      "order-3",
		Discount:     7.5,
		LegacyReason: "promo",
		Total:        legacyMoney{Amount: 10, Currency: "EUR", VAT: 21},
	}
	if _, err := store.Save(ctx, []cqrs.Envelope{{
		EventID:    uuid.New(),
		StreamID:   streamID,
		Event:      &legacy,
		OccurredAt: time.Now(),
		Metadata:   map[string]any{},
	}}, cqrs.NoStream{}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	iter, err := store.LoadStream(ctx, streamID)
	if err != nil {
		t.Fatalf("LoadStream: %v", err)
	}
	loaded := collectAll(t, iter)
	if len(loaded) != 1 {
		t.Fatalf("LoadStream returned %d events, want 1", len(loaded))
	}

	// The registry maps the stored name to the *current* struct.
	got, ok := loaded[0].Event.(*roundtripEvent)
	if !ok {
		t.Fatalf("loaded event is %T, want *roundtripEvent", loaded[0].Event)
	}
	if got.OrderID != "order-3" {
		t.Errorf("OrderID = %q, want %q", got.OrderID, "order-3")
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

// legacyPayloadEvent writes under roundtripEvent's name but with the field set
// an older build had: an extra "legacy_reason", an extra "vat" nested inside
// the total, and none of the fields added since.
type legacyPayloadEvent struct {
	OrderID      string      `json:"order_id"`
	Discount     float64     `json:"discount"`
	LegacyReason string      `json:"legacy_reason"`
	Total        legacyMoney `json:"total"`
}

type legacyMoney struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
	VAT      int    `json:"vat"`
}

func (e *legacyPayloadEvent) AggregateID() string { return e.OrderID }
func (e *legacyPayloadEvent) EventType() string   { return "roundtripEvent" }
