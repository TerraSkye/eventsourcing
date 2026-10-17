//go:build integration

package kurrentdb_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	cqrs "github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/eventsourcingtest"
	kdb "github.com/terraskye/eventsourcing/kurrentdb"

	"github.com/cenkalti/backoff/v4"
)

type CheckEvent struct {
	N int
}

func (CheckEvent) AggregateID() string { return "check" }
func (CheckEvent) EventType() string   { return "CheckEvent" }

func collectAll(t *testing.T, iter *cqrs.Iterator[*cqrs.Envelope]) []*cqrs.Envelope {
	t.Helper()
	var out []*cqrs.Envelope
	for iter.Next() {
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
	store := kdb.NewEventStore(testDB)
	ctx := context.Background()
	streamID := "check-inclusive"

	// Save 3 events (Versions 1, 2, 3; native KurrentDB revisions 0, 1, 2).
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
	lastVersion := all[len(all)-1].Version // 3, native EventNumber 2

	// Retry: resume from the last consumed revision. Since all 3 events
	// were already evolved, this MUST yield zero events.
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

// TestReadAll_ReachesTheEndOfAll pages through all of $all and requires
// the read to end with AtEnd, rather than block. It descends from GitHub
// issue #44, where a global read passed count=0 to
// (*kurrentdb.Client).ReadAll and blocked forever past the last event; and
// $all carries KurrentDB's own records, such as $metadata, which ReadAll
// must skip rather than fail to decode.
func TestReadAll_ReachesTheEndOfAll(t *testing.T) {
	store := kdb.NewEventStore(testDB)
	ctx := context.Background()
	streamID := "check-readall-end"

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

	done := make(chan error, 1)
	go func() {
		var after uint64
		for {
			res, err := store.ReadAll(ctx, after, cqrs.ReadOptions{Limit: 100})
			if err != nil {
				done <- err
				return
			}
			after = res.Next
			if res.AtEnd {
				done <- nil
				return
			}
		}
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ReadAll failed: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("paging through $all did not reach AtEnd within 15s")
	}
}

type BackoffProbeEvent struct{ N int }

func (BackoffProbeEvent) AggregateID() string { return "backoff-probe" }
func (BackoffProbeEvent) EventType() string   { return "BackoffProbeEvent" }

// TestSave_UsesConfiguredBackoffNotDefault verifies Save actually retries
// with the [kdb.Option] backoff it's given via [kdb.WithBackoff],
// rather than silently falling back to a fresh, unconfigured
// backoff.NewExponentialBackOff() (default MaxElapsedTime 15m). It uses a
// short test-only backoff instead of Save's real 30s default so the test
// runs in well under a second while still exercising the same code path.
func TestSave_UsesConfiguredBackoffNotDefault(t *testing.T) {
	const maxElapsed = 200 * time.Millisecond

	store := kdb.NewEventStore(testDB, kdb.WithBackoff(func() backoff.BackOff {
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

// TestLoadStream_ReturnsEveryEventInLargeStreams is a regression test for the
// silent truncation a hardcoded count=5000 caused.
//
// LoadStream promises "a lazy iterator over all events in the stream
// identified by id", but eventstore.go passed a literal 5000 as the `count`
// argument to (*kurrentdb.Client).ReadStream. That count is not a page size
// the client transparently re-requests past -- KurrentDB's
// ReadReq.Options.CountOption bounds the entire single-request read
// server-side (confirmed against the vendored client's
// toReadStreamRequest/readInternal/ReadStream.Recv: once `count` events are
// delivered the server ends the gRPC stream, which Recv reports as a plain
// io.EOF, identical to a real end of stream). A stream of more than 5000
// events yielded only its first 5000, with iter.Err() == nil --
// indistinguishable from a complete read.
//
// LoadStreamFrom was bounded the same way and takes the same fix; it is not
// covered separately here because it needs its own >5000-event stream.
func TestLoadStream_ReturnsEveryEventInLargeStreams(t *testing.T) {
	if testing.Short() {
		t.Skip("appends and reads back 5001 events")
	}

	store := kdb.NewEventStore(testDB)
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
	for iter.Next() {
		count++
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}

	if count != total {
		t.Errorf("LoadStream returned %d events for a %d-event stream, want %d "+
			"(a bounded count truncates the read server-side, and the client "+
			"reports it as a clean end of stream)", count, total, total)
	}
}

// TestLoadStreamFrom_HugeRevisionIsRefusedNotReplayed covers what an
// unrepresentable Revision does now.
//
// Revision is a uint64 and ToRawInt64 returns int64, so any Revision >= 1<<63
// used to sign-flip negative. Here that made LoadStreamFrom's `> 0` check
// false, so it fell through to kurrentdb.Start{} — the very beginning of the
// stream — and a caller resuming from what it believed was a huge,
// already-consumed revision got the whole stream replayed instead.
//
// ToRawInt64 now panics rather than returning a value that means something
// else, so the replay cannot happen. That is the loud half of the fix; a
// revision merely past the end of the stream (but below 1<<63) is a separate
// question — eventstore/memory rejects it with ErrInvalidRevision, and this
// store does not yet.
func TestLoadStreamFrom_HugeRevisionIsRefusedNotReplayed(t *testing.T) {

	store := kdb.NewEventStore(testDB)
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

	hugeRevision := cqrs.Revision(uint64(1) << 63)

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("LoadStreamFrom(id, Revision(1<<63)) did not panic: an " +
				"unrepresentable revision is being read as a marker value again, " +
				"which replays the stream from the beginning")
		}
		if msg, ok := r.(string); !ok || !strings.Contains(msg, "overflows int64") {
			t.Fatalf("panicked with %v, want the Revision overflow panic", r)
		}
	}()

	//nolint:errcheck // the call panics; the deferred check is the assertion
	store.LoadStreamFrom(ctx, streamID, hugeRevision)
}

// TestLoadStream_MissingStreamIsReportedNotFound is a regression test for
// .bug/eventstore-kurrentdb-loadstream-missing-stream-masked-as-empty.md.
//
// LoadStream and LoadStreamFrom used to discard every error from
// streamer.Recv and return io.EOF instead, which cqrs.Iterator translates
// into a clean end of iteration (Next() == false, Err() == nil). A stream
// that did not exist -- and equally a connection that dropped halfway
// through a read -- was indistinguishable from a complete, empty read.
// LoadStream now reports a missing stream as cqrs.ErrStreamNotFound, the
// same sentinel packages memory and file use.
func TestLoadStream_MissingStreamIsReportedNotFound(t *testing.T) {
	store := kdb.NewEventStore(testDB)
	ctx := context.Background()

	iter, err := store.LoadStream(ctx, "stream-that-does-not-exist")
	if err != nil {
		if !errors.Is(err, cqrs.ErrStreamNotFound) {
			t.Fatalf("LoadStream error = %v, want cqrs.ErrStreamNotFound", err)
		}
		return // reported at open time: acceptable
	}

	got := 0
	for iter.Next() {
		got++
	}
	if got != 0 {
		t.Fatalf("got %d events from a non-existent stream", got)
	}
	if err := iter.Err(); !errors.Is(err, cqrs.ErrStreamNotFound) {
		t.Fatalf("iter.Err() = %v, want cqrs.ErrStreamNotFound: a missing stream is "+
			"being reported as a clean, complete, empty read again, so callers cannot "+
			"tell it apart from an existing empty stream -- nor from a read that failed "+
			"partway through", err)
	}
}

// TestAcceptance does not Close the store: that would close testDB, which
// every other test in this package shares.
func TestAcceptance(t *testing.T) {
	eventsourcingtest.AcceptanceTest(t, kdb.NewEventStore(testDB))
}

func TestGlobalReaderAcceptance(t *testing.T) {
	store := kdb.NewEventStore(testDB)
	eventsourcingtest.GlobalReaderAcceptanceTest(t, store, store)
}
