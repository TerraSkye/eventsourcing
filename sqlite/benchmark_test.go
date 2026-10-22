//go:build bench

package sqlite_test

import (
	"context"
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
	projectiontest.Benchmark(b, func(b *testing.B) (cqrs.EventStore, projection.Store, cqrs.EventHandler) {
		db := openBenchmarkDB(b)
		if _, err := db.ExecContext(b.Context(), benchmarkRowsSchema); err != nil {
			b.Fatalf("create benchmark_rows: %v", err)
		}
		return sqlite.NewEventStore(db), sqlite.NewProjectionStore(db), cqrs.NewEventHandlerFunc(writeBenchmarkRow)
	})
}

// benchmarkRowsSchema is the benchmark's read model: one row per stream,
// holding the version last applied.
const benchmarkRowsSchema = `CREATE TABLE IF NOT EXISTS benchmark_rows (
	stream_id TEXT PRIMARY KEY,
	version   INTEGER NOT NULL
)`

// writeBenchmarkRow upserts the event's stream row in the projection's
// transaction, as a real projection writes its read model.
func writeBenchmarkRow(ctx context.Context, _ cqrs.Event) error {
	_, err := sqlite.Tx(ctx).ExecContext(ctx,
		`INSERT INTO benchmark_rows (stream_id, version) VALUES (?, ?)
		 ON CONFLICT (stream_id) DO UPDATE SET version = excluded.version`,
		cqrs.StreamIDFromContext(ctx), int64(cqrs.VersionFromContext(ctx)))
	return err
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
