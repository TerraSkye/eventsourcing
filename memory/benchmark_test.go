//go:build bench

package memory_test

import (
	"testing"

	cqrs "github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/eventsourcingtest"
	"github.com/terraskye/eventsourcing/memory"
	"github.com/terraskye/eventsourcing/projection"
	"github.com/terraskye/eventsourcing/projection/projectiontest"
)

func BenchmarkEventStore(b *testing.B) {
	eventsourcingtest.Benchmark(b, func(*testing.B) cqrs.EventStore {
		return memory.NewEventStore(0)
	})
}

func BenchmarkProjection(b *testing.B) {
	projectiontest.Benchmark(b, func(*testing.B) (cqrs.EventStore, projection.Store) {
		return memory.NewEventStore(0), memory.NewProjectionStore()
	})
}
