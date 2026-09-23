package kurrentdb_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"testing"
	"time"

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
		for iter.Next() {
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
	for iter.Next() {
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
