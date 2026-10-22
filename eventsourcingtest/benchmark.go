package eventsourcingtest

import (
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
)

// benchmarkEvent is the only event type the benchmarks save. It carries a
// small payload, so encoding and decoding cost something, as with a real
// event.
type benchmarkEvent struct {
	ID    string
	N     int
	Title string
}

func (e *benchmarkEvent) AggregateID() string { return e.ID }
func (e *benchmarkEvent) EventType() string   { return "eventsourcingtest.benchmarkEvent" }

func init() {
	eventsourcing.RegisterEvent(&benchmarkEvent{})
}

const (
	// seedBatch is how many events one Save writes while seeding: large
	// enough to seed quickly, small enough to stay under every backend's
	// limit on query parameters.
	seedBatch = 100

	// concurrentStreams and concurrentWriters shape the contended append:
	// many writers, each appending to streams of its own, as many command
	// handlers serving different aggregates do.
	concurrentStreams = 1_000
	concurrentWriters = 32

	// readAllEvents is how many events the ReadAll benchmarks page through.
	readAllEvents = 100_000
)

// loadLengths are the stream lengths LoadStream is measured at.
var loadLengths = []int{10, 100, 1_000, 10_000}

// readAllLimits are the ReadOptions.Limit values ReadAll is measured at:
// the projection package's default batch size, and one either side of it.
var readAllLimits = []int{100, 500, 2_000}

// Benchmark measures the [eventsourcing.EventStore] that newStore returns
// as a set of sub-benchmarks, so that every implementation is measured by
// the same code:
//
//   - Append: one event per Save, first to a single stream from one
//     writer, then to 1,000 streams from 32 concurrent writers. The
//     second is where a store that serializes appends, such as the
//     Postgres store under its append lock, falls behind.
//   - LoadStream: reading a whole stream of 10, 100, 1,000 and 10,000
//     events, the cost of rebuilding an aggregate without a snapshot.
//   - ReadAll: paging through 100,000 events of the global log at a Limit
//     of 100, 500 and 2,000, the ceiling on how fast any projection can
//     catch up. It runs only when the store is also an
//     [eventsourcing.GlobalReader].
//
// Each reports its throughput as an events/s metric, which compares across
// sub-benchmarks where ns/op does not.
//
// newStore is called once per group of sub-benchmarks. The store it
// returns may already hold events and may be shared: the benchmarks write
// to streams of their own, named with a fresh UUID, and read the global log
// only after the head they found before seeding. A store that starts empty
// gives steadier numbers, since a growing table slows some backends down.
// Benchmark never calls Close; that is left to the caller.
//
// A backend's benchmark is a few lines:
//
//	func BenchmarkEventStore(b *testing.B) {
//		eventsourcingtest.Benchmark(b, func(*testing.B) eventsourcing.EventStore {
//			return memory.NewEventStore(0)
//		})
//	}
func Benchmark(b *testing.B, newStore func(b *testing.B) eventsourcing.EventStore) {
	b.Helper()

	b.Run("Append", func(b *testing.B) {
		store := newStore(b)
		b.Run("Streams=1/Writers=1", func(b *testing.B) {
			benchmarkAppend(b, store, 1, 1)
		})
		b.Run(fmt.Sprintf("Streams=%d/Writers=%d", concurrentStreams, concurrentWriters), func(b *testing.B) {
			benchmarkAppend(b, store, concurrentStreams, concurrentWriters)
		})
	})

	b.Run("LoadStream", func(b *testing.B) {
		store := newStore(b)
		ids := make(map[int]string, len(loadLengths))
		for _, n := range loadLengths {
			ids[n] = seedStream(b, store, n)
		}
		for _, n := range loadLengths {
			b.Run("Events="+strconv.Itoa(n), func(b *testing.B) {
				benchmarkLoadStream(b, store, ids[n], n)
			})
		}
	})

	b.Run("ReadAll", func(b *testing.B) {
		store := newStore(b)
		reader, ok := store.(eventsourcing.GlobalReader)
		if !ok {
			b.Skipf("%T does not implement eventsourcing.GlobalReader", store)
		}
		after, err := reader.Head(b.Context())
		if err != nil {
			b.Fatalf("Head: %v", err)
		}
		for range readAllEvents / seedBatch {
			seedStream(b, store, seedBatch)
		}
		for _, limit := range readAllLimits {
			b.Run("Limit="+strconv.Itoa(limit), func(b *testing.B) {
				benchmarkReadAll(b, reader, after, limit)
			})
		}
	})
}

// benchmarkAppend appends b.N events, one per Save, across streams new
// streams from writers goroutines. Each writer owns every writers-th
// stream and tracks its revision, so Saves state the exact revision the
// way [eventsourcing.NewCommandHandler] does, Revision(0) for a new
// stream, and never conflict.
//
// It uses b.N rather than b.Loop: the writers share the b.N appends between
// them, and b.Loop cannot be split across goroutines.
func benchmarkAppend(b *testing.B, store eventsourcing.EventStore, streams, writers int) {
	ids := make([]string, streams)
	for i := range ids {
		ids[i] = newBenchmarkStreamID()
	}

	var (
		next     atomic.Int64
		wg       sync.WaitGroup
		failOnce sync.Once
		failure  error
	)
	b.ResetTimer()
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// This writer's streams are ids[w], ids[w+writers], and so on.
			var owned []string
			for i := w; i < streams; i += writers {
				owned = append(owned, ids[i])
			}
			versions := make([]uint64, len(owned))
			for i := 0; next.Add(1) <= int64(b.N); i++ {
				s := i % len(owned)
				env := benchmarkEnvelope(owned[s], versions[s]+1)
				if _, err := store.Save(b.Context(), []eventsourcing.Envelope{env}, eventsourcing.Revision(versions[s])); err != nil {
					failOnce.Do(func() { failure = fmt.Errorf("Save to %q at revision %d: %w", owned[s], versions[s], err) })
					return
				}
				versions[s]++
			}
		}()
	}
	wg.Wait()
	b.StopTimer()
	if failure != nil {
		b.Fatal(failure)
	}
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "events/s")
}

// benchmarkLoadStream reads the whole of stream id, which holds n events,
// once per iteration.
func benchmarkLoadStream(b *testing.B, store eventsourcing.EventStore, id string, n int) {
	for b.Loop() {
		it, err := store.LoadStream(b.Context(), id)
		if err != nil {
			b.Fatalf("LoadStream(%q): %v", id, err)
		}
		got := 0
		for it.Next() {
			got++
		}
		if err := it.Err(); err != nil {
			b.Fatalf("LoadStream(%q): iterate: %v", id, err)
		}
		if got != n {
			b.Fatalf("LoadStream(%q) returned %d events, want %d", id, got, n)
		}
	}
	b.ReportMetric(float64(n)*float64(b.N)/b.Elapsed().Seconds(), "events/s")
}

// benchmarkReadAll pages through the readAllEvents events after position
// after, limit events at a time, once per iteration, filtered on the
// benchmark's event type the way a projection filters on its EventTypes.
func benchmarkReadAll(b *testing.B, reader eventsourcing.GlobalReader, after uint64, limit int) {
	opts := eventsourcing.ReadOptions{
		Limit:      limit,
		EventTypes: []string{(&benchmarkEvent{}).EventType()},
	}
	for b.Loop() {
		pos, got := after, 0
		for got < readAllEvents {
			res, err := reader.ReadAll(b.Context(), pos, opts)
			if err != nil {
				b.Fatalf("ReadAll(after %d): %v", pos, err)
			}
			got += len(res.Events)
			pos = res.Next
			if res.AtEnd {
				break
			}
		}
		if got != readAllEvents {
			b.Fatalf("ReadAll paged through %d events, want %d", got, readAllEvents)
		}
	}
	b.ReportMetric(float64(readAllEvents)*float64(b.N)/b.Elapsed().Seconds(), "events/s")
}

// seedStream creates a new stream holding n events and returns its ID.
func seedStream(b *testing.B, store eventsourcing.EventStore, n int) string {
	b.Helper()
	id := newBenchmarkStreamID()
	for from := 0; from < n; from += seedBatch {
		batch := make([]eventsourcing.Envelope, min(seedBatch, n-from))
		for i := range batch {
			batch[i] = benchmarkEnvelope(id, uint64(from+i+1))
		}
		if _, err := store.Save(b.Context(), batch, eventsourcing.Revision(from)); err != nil {
			b.Fatalf("seed %q: Save at revision %d: %v", id, from, err)
		}
	}
	return id
}

// newBenchmarkStreamID returns a stream ID no other benchmark, or earlier
// run against a persistent store, has used.
func newBenchmarkStreamID() string {
	return "benchmark-" + uuid.NewString()
}

// benchmarkEnvelope returns the envelope for event version of stream id,
// with metadata and a chain like one [eventsourcing.NewCommandHandler]
// appends.
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
