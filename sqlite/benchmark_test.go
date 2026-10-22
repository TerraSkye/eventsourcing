//go:build bench

package sqlite_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	cqrs "github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/eventsourcingtest"
	"github.com/terraskye/eventsourcing/projection"
	"github.com/terraskye/eventsourcing/projection/projectiontest"
	"github.com/terraskye/eventsourcing/sqlite"
)

// The benchmarks use a database file, as in production: an in-memory
// database skips the write-ahead log and the disk.

func BenchmarkEventStore(b *testing.B) {
	eventsourcingtest.Benchmark(b, func(b *testing.B) cqrs.EventStore {
		return sqlite.NewEventStore(openBenchmarkDB(b))
	})
}

func BenchmarkProjection(b *testing.B) {
	projectiontest.Benchmark(b, func(b *testing.B) (cqrs.EventStore, projection.Store) {
		db := openBenchmarkDB(b)
		return sqlite.NewEventStore(db), sqlite.NewProjectionStore(db)
	})
}

// openBenchmarkDB opens a new database file with the schema applied, closed
// when the benchmark ends.
func openBenchmarkDB(b *testing.B) *sql.DB {
	b.Helper()
	db, err := sqlite.Open(filepath.Join(b.TempDir(), "events.db"))
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(b.Context(), sqlite.Schema); err != nil {
		b.Fatalf("apply schema: %v", err)
	}
	return db
}
