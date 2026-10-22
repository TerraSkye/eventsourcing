package projectiontest

import (
	"context"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/projection"
)

// benchmarkEvent is the only event type the benchmarks save and project.
type benchmarkEvent struct {
	ID    string
	N     int
	Title string
}

func (e *benchmarkEvent) AggregateID() string { return e.ID }
func (e *benchmarkEvent) EventType() string   { return "projectiontest.benchmarkEvent" }

func init() {
	eventsourcing.RegisterEvent(&benchmarkEvent{})
}

const (
	// catchUpStreams and catchUpStreamLength make the log the catch-up
	// benchmarks replay: 100,000 events, spread over enough streams that
	// every segment gets its share.
	catchUpStreams      = 1_000
	catchUpStreamLength = 100

	// benchmarkTimeout bounds one wait for a runner, so a runner that stops
	// making progress fails the benchmark instead of hanging it.
	benchmarkTimeout = 10 * time.Minute
)

// catchUpSegments are the segment counts the catch-up is measured at.
var catchUpSegments = []int{1, 4, 16}

// Benchmark measures a [projection.Runner] reading the event store, and
// keeping its checkpoints in the [projection.Store], that newStores
// returns, as a set of sub-benchmarks:
//
//   - CatchUp: a projection with a no-op handler catching up on 100,000
//     events, at 1, 4 and 16 segments. It reports events/s, the events in
//     the log over the time to process all of them, and reads/event, the
//     events the store returned to every segment together for each event
//     in the log. Segments parallelize the handlers, not the reads, so
//     reads/event comes out at the number of segments.
//   - CommandToLive: the time from appending one event to the projection
//     having processed it, through Save, [projection.Runner.Nudge] and
//     [projection.Runner.WaitUntil], as an application that nudges its
//     runners on every append sees it. It reports the median and the 99th
//     percentile as p50-ms and p99-ms.
//
// newStores is called once per group of sub-benchmarks, and must return an
// event store that implements [eventsourcing.GlobalReader], together with
// a projection store, typically on the same database. The event store
// should be empty: the projections read the whole log, and the benchmarks
// count only the events they appended. Each projection is named with a
// fresh UUID, so the projection store may be shared. Benchmark never calls
// Close; that is left to the caller.
//
// A backend's benchmark is a few lines:
//
//	func BenchmarkProjection(b *testing.B) {
//		projectiontest.Benchmark(b, func(*testing.B) (eventsourcing.EventStore, projection.Store) {
//			return memory.NewEventStore(0), memory.NewProjectionStore()
//		})
//	}
func Benchmark(b *testing.B, newStores func(b *testing.B) (eventsourcing.EventStore, projection.Store)) {
	b.Helper()

	b.Run("CatchUp", func(b *testing.B) {
		events, store := newStores(b)
		reader := globalReader(b, events)
		for range catchUpStreams {
			seedStream(b, events, catchUpStreamLength)
		}
		head, err := reader.Head(b.Context())
		if err != nil {
			b.Fatalf("Head: %v", err)
		}
		for _, segments := range catchUpSegments {
			b.Run("Segments="+strconv.Itoa(segments), func(b *testing.B) {
				benchmarkCatchUp(b, reader, store, head, segments)
			})
		}
	})

	b.Run("CommandToLive", func(b *testing.B) {
		events, store := newStores(b)
		benchmarkCommandToLive(b, events, globalReader(b, events), store)
	})
}

// benchmarkCatchUp runs a new projection of segments segments from the
// start of the log to head once per iteration. Only the time from Run to
// every segment reaching head is measured; building the runner and
// stopping it, which releases its claims, are not.
func benchmarkCatchUp(b *testing.B, reader eventsourcing.GlobalReader, store projection.Store, head uint64, segments int) {
	var handled atomic.Int64
	handler := eventsourcing.NewEventHandlerFunc(func(context.Context, eventsourcing.Event) error {
		handled.Add(1)
		return nil
	})
	counting := &countingReader{GlobalReader: reader}

	for b.Loop() {
		b.StopTimer()
		r := newBenchmarkRunner(handler, counting, store, segments)
		changed := watch(r)
		ctx, cancel := context.WithCancel(b.Context())
		done := make(chan struct{})
		b.StartTimer()

		go func() {
			defer close(done)
			_ = r.Run(ctx)
		}()
		err := waitPosition(b.Context(), r, changed, head)

		b.StopTimer()
		cancel()
		<-done
		if err != nil {
			b.Fatalf("catch up to %d with %d segments: %v", head, segments, err)
		}
		b.StartTimer()
	}

	n := float64(handled.Load())
	if n == 0 {
		b.Fatal("the projection handled no events")
	}
	b.ReportMetric(n/b.Elapsed().Seconds(), "events/s")
	b.ReportMetric(float64(counting.read.Load())/n, "reads/event")
}

// benchmarkCommandToLive appends one event per iteration to a projection
// that is already live, and waits until the projection has processed it.
func benchmarkCommandToLive(b *testing.B, events eventsourcing.EventStore, reader eventsourcing.GlobalReader, store projection.Store) {
	handler := eventsourcing.NewEventHandlerFunc(func(context.Context, eventsourcing.Event) error {
		return nil
	})
	r := newBenchmarkRunner(handler, reader, store, 1)
	changed := watch(r)
	ctx, cancel := context.WithCancel(b.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.Run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	// Wait until the runner holds its segment and has processed a first
	// event, so WaitUntil below watches memory rather than the store.
	id := "benchmark-" + uuid.NewString()
	res, err := events.Save(b.Context(), []eventsourcing.Envelope{benchmarkEnvelope(id, 1)}, eventsourcing.Revision(0))
	if err != nil {
		b.Fatalf("Save: %v", err)
	}
	r.Nudge()
	if err := waitPosition(b.Context(), r, changed, res.GlobalPosition); err != nil {
		b.Fatalf("wait for the projection to go live: %v", err)
	}

	var latencies []time.Duration
	version := uint64(1)
	for b.Loop() {
		start := time.Now()
		res, err := events.Save(b.Context(), []eventsourcing.Envelope{benchmarkEnvelope(id, version+1)}, eventsourcing.Revision(version))
		if err != nil {
			b.Fatalf("Save at revision %d: %v", version, err)
		}
		r.Nudge()
		waitCtx, waitCancel := context.WithTimeout(b.Context(), benchmarkTimeout)
		err = r.WaitUntil(waitCtx, res.GlobalPosition)
		waitCancel()
		if err != nil {
			b.Fatalf("WaitUntil(%d): %v", res.GlobalPosition, err)
		}
		latencies = append(latencies, time.Since(start))
		version++
	}

	slices.Sort(latencies)
	b.ReportMetric(milliseconds(percentile(latencies, 0.50)), "p50-ms")
	b.ReportMetric(milliseconds(percentile(latencies, 0.99)), "p99-ms")
}

// newBenchmarkRunner returns a runner for a new projection, segmented by
// stream when segments is above 1, that logs nothing.
func newBenchmarkRunner(handler eventsourcing.EventHandler, reader eventsourcing.GlobalReader, store projection.Store, segments int) *projection.Runner {
	return projection.NewRunner(projection.Definition{
		Name:       "benchmark-" + uuid.NewString(),
		Handler:    handler,
		EventTypes: []string{(&benchmarkEvent{}).EventType()},
		Segments:   segments,
		SegmentKey: projection.ByStreamID,
		Logger:     slog.New(slog.DiscardHandler),
	}, reader, store)
}

// watch returns a channel that receives a value, without blocking the
// runner, whenever r commits a batch of events or a segment changes phase.
func watch(r *projection.Runner) <-chan struct{} {
	changed := make(chan struct{}, 1)
	r.Watch(func() {
		select {
		case changed <- struct{}{}:
		default: // a wake-up is already pending
		}
	})
	return changed
}

// waitPosition waits until every segment of r has processed the log up to
// position. Unlike [projection.Runner.WaitUntil], it never falls back to
// polling the store while r is still claiming its segments, so the time it
// takes is the runner's alone.
//
// It relies on the last batch a segment commits before reaching position
// either holding events or turning the segment [projection.Live], both of
// which wake changed.
func waitPosition(ctx context.Context, r *projection.Runner, changed <-chan struct{}, position uint64) error {
	ctx, cancel := context.WithTimeout(ctx, benchmarkTimeout)
	defer cancel()
	for r.Status().Position < position {
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// countingReader counts the events ReadAll returns, across every segment
// reading through it.
type countingReader struct {
	eventsourcing.GlobalReader
	read atomic.Int64
}

func (c *countingReader) ReadAll(ctx context.Context, after uint64, opts eventsourcing.ReadOptions) (eventsourcing.ReadResult, error) {
	res, err := c.GlobalReader.ReadAll(ctx, after, opts)
	c.read.Add(int64(len(res.Events)))
	return res, err
}

// globalReader returns events as an [eventsourcing.GlobalReader], or fails
// the benchmark when it is not one.
func globalReader(b *testing.B, events eventsourcing.EventStore) eventsourcing.GlobalReader {
	b.Helper()
	reader, ok := events.(eventsourcing.GlobalReader)
	if !ok {
		b.Fatalf("%T does not implement eventsourcing.GlobalReader", events)
	}
	return reader
}

// seedStream creates a new stream holding n events, saved in one batch.
func seedStream(b *testing.B, events eventsourcing.EventStore, n int) {
	b.Helper()
	id := "benchmark-" + uuid.NewString()
	batch := make([]eventsourcing.Envelope, n)
	for i := range batch {
		batch[i] = benchmarkEnvelope(id, uint64(i+1))
	}
	if _, err := events.Save(b.Context(), batch, eventsourcing.Revision(0)); err != nil {
		b.Fatalf("seed %q: %v", id, err)
	}
}

// benchmarkEnvelope returns the envelope for event version of stream id.
func benchmarkEnvelope(id string, version uint64) eventsourcing.Envelope {
	return eventsourcing.Envelope{
		EventID:       uuid.New(),
		StreamID:      id,
		Event:         &benchmarkEvent{ID: id, N: int(version), Title: "Write the benchmark suite"},
		Metadata:      map[string]any{"command_type": "benchmark"},
		Version:       version,
		OccurredAt:    time.Now(),
		CorrelationID: id,
		CausationID:   uuid.NewString(),
	}
}

// percentile returns the nearest-rank p-th percentile of sorted, or 0 when
// it is empty.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[max(i, 0)]
}

func milliseconds(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}
