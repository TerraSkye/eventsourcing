//go:build bench

package kurrentdb_test

import (
	"testing"

	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/eventsourcingtest"
	kdb "github.com/terraskye/eventsourcing/kurrentdb"
)

// KurrentDB has no projection.Store, so only the event store is measured.
// Every benchmark shares the one container; eventsourcingtest.Benchmark
// writes to streams of its own and reads only after the head it found.

func BenchmarkEventStore(b *testing.B) {
	eventsourcingtest.Benchmark(b, func(*testing.B) eventsourcing.EventStore {
		return kdb.NewEventStore(testDB)
	})
}
