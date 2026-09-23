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

// TestLoadStream_ReturnsEveryEventInLargeStreams is a regression test for
// the silent truncation that eventstore.go's hardcoded count=5000 caused.
//
// LoadStream's doc comment promises "a lazy iterator over all events in the
// stream identified by id", but all three Load* methods passed a literal
// 5000 as the `count` argument to (*kurrentdb.Client).ReadStream/ReadAll.
// That count is not a page/batch size the client transparently re-requests
// past -- KurrentDB's ReadReq.Options.CountOption bounds the entire
// single-request read server-side (confirmed by reading the vendor client's
// toReadStreamRequest/readInternal/ReadStream.Recv: once `count` events are
// delivered the server ends the gRPC stream, which Recv reports as a plain
// io.EOF, identical to a real end-of-stream). So a stream with more than
// 5000 events yielded only its first 5000 to the caller, with iter.Err() ==
// nil -- indistinguishable from a normal, complete read.
//
// The fix is eventstore.go's readToEnd count. LoadStreamFrom and LoadFromAll
// were bounded the same way and take the same fix, but are not covered
// separately here: every such case needs its own >5000-event stream.
//
// This test is intentionally slow -- it appends 5001 events and reads them
// back -- so it is skipped under -short.
func TestLoadStream_ReturnsEveryEventInLargeStreams(t *testing.T) {
	if testing.Short() {
		t.Skip("appends and reads back 5001 events")
	}

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
			"(the count passed to (*kurrentdb.Client).ReadStream is bounding the read "+
			"below the stream's length again, silently truncating it instead of "+
			"returning every event as LoadStream's doc comment promises)",
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

// TestSave_RevisionConflictNotTranslatedToConflictError documents the bug
// filed as .bug/eventstore-kurrentdb-save-revision-conflict-not-translated.md.
//
// Save tries to translate an optimistic-concurrency failure with
// `errors.As(err, &conflictErr)` against *kurrentdb.StreamRevisionConflictError.
// The client only produces that type from rich gRPC status details
// (ErrorCodeStreamRevisionConflict, code 8); a plain failed append returns
// &Error{code: ErrorCodeWrongExpectedVersion} (code 7) wrapping a bare
// fmt.Errorf (client.go:147). The errors.As therefore never matches, Save
// returns a generic wrapped error, and no caller can detect the conflict --
// including NewCommandHandler, whose retry loop keys on
// errors.As(err, &conflict) and otherwise wraps the failure in
// backoff.Permanent. Optimistic-concurrency retries silently never happen
// against this store, while eventstore/memory returns a proper
// *cqrs.StreamRevisionConflictError with both revisions populated.
func TestSave_RevisionConflictNotTranslatedToConflictError(t *testing.T) {
	t.Skip("documents bug: eventstore-kurrentdb-save-revision-conflict-not-translated, see .bug/eventstore-kurrentdb-save-revision-conflict-not-translated.md")

	store := kdbstore.NewEventStore(testDB)
	ctx := context.Background()
	streamID := "conflict-translation"

	for i := 0; i < 2; i++ {
		if _, err := store.Save(ctx, []cqrs.Envelope{{
			StreamID: streamID, Event: &CheckEvent{N: i},
			Metadata: map[string]any{}, OccurredAt: time.Now(),
		}}, cqrs.Any{}); err != nil {
			t.Fatalf("seed save %d: %v", i, err)
		}
	}

	// The stream is at revision 1; asserting revision 0 is a genuine
	// optimistic-concurrency conflict.
	_, err := store.Save(ctx, []cqrs.Envelope{{
		StreamID: streamID, Event: &CheckEvent{N: 99},
		Metadata: map[string]any{}, OccurredAt: time.Now(),
	}}, cqrs.Revision(0))
	if err == nil {
		t.Fatal("expected a revision conflict, got nil")
	}

	var conflict *cqrs.StreamRevisionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("Save returned %v, which is not a *cqrs.StreamRevisionConflictError: "+
			"NewCommandHandler's retry loop keys on errors.As(err, &conflict), so a "+
			"concurrency conflict is reported as a permanent failure and never retried", err)
	}
	// Whoever fixes the detection must populate both revisions: Error() calls
	// ToRawInt64() on each, so a nil StreamState panics.
	if conflict.ExpectedRevision == nil || conflict.ActualRevision == nil {
		t.Fatalf("conflict has nil revisions (expected=%v actual=%v); Error() panics on nil",
			conflict.ExpectedRevision, conflict.ActualRevision)
	}
	if got := conflict.ActualRevision.ToRawInt64(); got != 1 {
		t.Errorf("ActualRevision = %d, want 1", got)
	}
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
// same sentinel eventstore/memory and eventstore/file use.
func TestLoadStream_MissingStreamIsReportedNotFound(t *testing.T) {
	store := kdbstore.NewEventStore(testDB)
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

// TestLoadStreamFrom_RevisionZeroRedeliversFirstEvent documents the bug filed
// as .bug/eventstore-kurrentdb-loadstreamfrom-revision-zero-redelivers-first-event.md.
//
// This is the same off-by-one as GitHub issue #45 (see
// TestLoadStreamFrom_RevisionIsInclusiveNotExclusive), still live for
// Revision(0). LoadStreamFrom converts the exclusive cqrs.Revision(N) to
// KurrentDB's inclusive read position with `if version.ToRawInt64() > 0`,
// so Revision(0) fails the guard and falls through to kurrentdb.Start{} --
// re-delivering version 0, the event the caller said it had already
// consumed. The code comment claims Start{} "already means the same thing",
// which holds only under a count-based reading of Revision, not the
// exclusive one the passing issue-#45 test pins.
func TestLoadStreamFrom_RevisionZeroRedeliversFirstEvent(t *testing.T) {
	t.Skip("documents bug: eventstore-kurrentdb-loadstreamfrom-revision-zero-redelivers-first-event, see .bug/eventstore-kurrentdb-loadstreamfrom-revision-zero-redelivers-first-event.md")

	store := kdbstore.NewEventStore(testDB)
	ctx := context.Background()
	streamID := "revision-zero-redelivery"

	// One event, at native KurrentDB revision 0.
	if _, err := store.Save(ctx, []cqrs.Envelope{{
		StreamID: streamID, Event: &CheckEvent{N: 0},
		Metadata: map[string]any{}, OccurredAt: time.Now(),
	}}, cqrs.Any{}); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Revision(0) means "version 0 already consumed, resume strictly after
	// it" -- the contract NewCommandHandler relies on via
	// `revision = Revision(event.Version)`. It must yield nothing.
	iter, err := store.LoadStreamFrom(ctx, streamID, cqrs.Revision(0))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := collectAll(t, iter)
	if len(got) != 0 {
		t.Errorf("LoadStreamFrom(id, Revision(0)) returned %d event(s) (version %d), want 0: "+
			"the already-consumed first event is re-delivered, so a command retried after a "+
			"conflict evolves it into the aggregate twice",
			len(got), got[0].Version)
	}
}

// TestLoadFromAll_IgnoresRequestedVersion documents the bug filed as
// .bug/eventstore-kurrentdb-loadfromall-ignores-version.md.
//
// LoadFromAll accepts a version but never reads it: it hardcodes
// From: kurrentdb.Start{} (the `//TODO fix `from“ in eventstore.go). Every
// call replays the whole $all stream from the beginning, so a projection or
// subscription cannot resume from where it left off -- it reprocesses every
// event ever stored. The EventStore interface documents LoadFromAll as
// returning "an iterator over events across every stream, starting at
// version".
//
// The proof is the error the read dies on: a position past the end of $all
// cannot reach the system events that sit at its very beginning, so seeing
// one decode-fail is itself evidence the read started at Start{}.
func TestLoadFromAll_IgnoresRequestedVersion(t *testing.T) {
	t.Skip("documents bug: eventstore-kurrentdb-loadfromall-ignores-version, see .bug/eventstore-kurrentdb-loadfromall-ignores-version.md")

	store := kdbstore.NewEventStore(testDB)
	ctx := context.Background()
	streamID := "loadfromall-version"

	for i := 0; i < 3; i++ {
		if _, err := store.Save(ctx, []cqrs.Envelope{{
			StreamID: streamID, Event: &CheckEvent{N: i},
			Metadata: map[string]any{}, OccurredAt: time.Now(),
		}}, cqrs.Any{}); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}

	iter, err := store.LoadFromAll(ctx, cqrs.Revision(1<<40))
	if err != nil {
		t.Fatalf("load from all: %v", err)
	}

	got := 0
	for iter.Next() {
		got++
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("LoadFromAll(Revision(1<<40)) failed with %v; a read starting past the end "+
			"of $all could not have reached that event, so `version` was ignored and the "+
			"read started at kurrentdb.Start{}", err)
	}
	if got != 0 {
		t.Errorf("LoadFromAll(Revision(1<<40)) returned %d events, want 0 for a position "+
			"beyond the end of $all", got)
	}
}

// TestLoadFromAll_FailsOnSystemEvents documents the bug filed as
// .bug/eventstore-kurrentdb-loadfromall-fails-on-system-events.md.
//
// LoadFromAll reads the $all stream, which carries KurrentDB's own system
// events ($metadata, $statsCollected, ...), and calls cqrs.NewEventByName on
// every one of them. Nothing registers those types, so the first system
// event ends the iteration with "event not registered", making LoadFromAll
// unusable for its documented purpose against a real server.
// TestLoadFromAll_HangsPastLastEvent already tolerates this error in passing;
// this test asserts it should not happen at all.
func TestLoadFromAll_FailsOnSystemEvents(t *testing.T) {
	t.Skip("documents bug: eventstore-kurrentdb-loadfromall-fails-on-system-events, see .bug/eventstore-kurrentdb-loadfromall-fails-on-system-events.md")

	store := kdbstore.NewEventStore(testDB)
	ctx := context.Background()
	streamID := "loadfromall-system-events"

	if _, err := store.Save(ctx, []cqrs.Envelope{{
		StreamID: streamID, Event: &CheckEvent{N: 1},
		Metadata: map[string]any{}, OccurredAt: time.Now(),
	}}, cqrs.Any{}); err != nil {
		t.Fatalf("save: %v", err)
	}

	iter, err := store.LoadFromAll(ctx, cqrs.Any{})
	if err != nil {
		t.Fatalf("load from all: %v", err)
	}
	for iter.Next() {
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("LoadFromAll over $all ended with %v: KurrentDB's own system events are "+
			"passed to cqrs.NewEventByName, which has no registration for them, so the "+
			"iteration dies instead of skipping them", err)
	}
}

// TestLoadStreamFrom_MissingStreamIsAnEmptyRead pins the other half of the
// error-mapping contract: LoadStreamFrom does not enforce existence
// preconditions, so an absent stream must stay an empty, successful read.
// This is what lets a command handler load a brand-new aggregate, and it is
// how Any{} behaves in the memory and file implementations -- mapping the
// client's not-found error to cqrs.ErrStreamNotFound here would break every
// first command for a new stream.
func TestLoadStreamFrom_MissingStreamIsAnEmptyRead(t *testing.T) {
	store := kdbstore.NewEventStore(testDB)
	ctx := context.Background()

	for _, version := range []cqrs.StreamState{cqrs.Any{}, cqrs.NoStream{}, cqrs.Revision(0)} {
		iter, err := store.LoadStreamFrom(ctx, "stream-that-does-not-exist-either", version)
		if err != nil {
			t.Fatalf("LoadStreamFrom(%T) error = %v, want an empty read", version, err)
		}
		got := collectAll(t, iter)
		if len(got) != 0 {
			t.Errorf("LoadStreamFrom(%T) returned %d events for a missing stream, want 0", version, len(got))
		}
	}
}

// TestSave_ExpectationFailuresMapToSentinels pins the Save half of the
// mapping: a violated NoStream or StreamExists expectation is reported with
// the same cqrs sentinel the memory and file implementations use, rather than
// as an opaque KurrentDB error. It also checks the client's own error stays
// reachable in the chain, since that is where the detail lives.
func TestSave_ExpectationFailuresMapToSentinels(t *testing.T) {
	store := kdbstore.NewEventStore(testDB)
	ctx := context.Background()

	env := func(streamID string, n int) []cqrs.Envelope {
		return []cqrs.Envelope{{
			StreamID: streamID, Event: &CheckEvent{N: n},
			Metadata: map[string]any{}, OccurredAt: time.Now(),
		}}
	}

	t.Run("NoStream on an existing stream is ErrStreamExists", func(t *testing.T) {
		streamID := "save-expect-nostream"
		if _, err := store.Save(ctx, env(streamID, 0), cqrs.Any{}); err != nil {
			t.Fatalf("seed save: %v", err)
		}

		_, err := store.Save(ctx, env(streamID, 1), cqrs.NoStream{})
		if !errors.Is(err, cqrs.ErrStreamExists) {
			t.Fatalf("Save error = %v, want cqrs.ErrStreamExists", err)
		}
		var kErr *kurrentdb.Error
		if !errors.As(err, &kErr) {
			t.Errorf("Save error = %v, want the KurrentDB error retained in the chain", err)
		}
	})

	t.Run("StreamExists on a missing stream is ErrStreamNotFound", func(t *testing.T) {
		_, err := store.Save(ctx, env("save-expect-streamexists-missing", 0), cqrs.StreamExists{})
		if !errors.Is(err, cqrs.ErrStreamNotFound) {
			t.Fatalf("Save error = %v, want cqrs.ErrStreamNotFound", err)
		}
	})

	t.Run("a successful save reports no error", func(t *testing.T) {
		if _, err := store.Save(ctx, env("save-expect-happy", 0), cqrs.NoStream{}); err != nil {
			t.Fatalf("Save on a new stream with NoStream{} = %v, want nil", err)
		}
	})
}
