//go:build bench

package memory_test

import (
	"context"
	"sync"
	"testing"

	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/eventsourcingtest"
	"github.com/terraskye/eventsourcing/memory"
	"github.com/terraskye/eventsourcing/projection"
	"github.com/terraskye/eventsourcing/projection/projectiontest"
)

func BenchmarkEventStore(b *testing.B) {
	eventsourcingtest.Benchmark(b, func(*testing.B) eventsourcing.EventStore {
		return memory.NewEventStore()
	})
}

func BenchmarkProjection(b *testing.B) {
	projectiontest.Benchmark(b, func(*testing.B) (eventsourcing.EventStore, projection.Store, eventsourcing.EventHandler) {
		return memory.NewEventStore(), memory.NewProjectionStore(), newRowsHandler()
	})
}

// newRowsHandler returns a handler that keeps the benchmark's read model in
// a map, the memory backend's database: one row per stream, holding the
// version last applied.
func newRowsHandler() eventsourcing.EventHandler {
	var mu sync.Mutex
	rows := map[string]uint64{}
	return eventsourcing.NewEventHandlerFunc(func(ctx context.Context, _ eventsourcing.Event) error {
		mu.Lock()
		defer mu.Unlock()
		rows[eventsourcing.StreamIDFromContext(ctx)] = eventsourcing.VersionFromContext(ctx)
		return nil
	})
}
