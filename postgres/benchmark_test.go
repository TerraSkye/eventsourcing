//go:build bench

package postgres_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/eventsourcingtest"
	"github.com/terraskye/eventsourcing/postgres"
	"github.com/terraskye/eventsourcing/projection"
	"github.com/terraskye/eventsourcing/projection/projectiontest"
)

// benchmarkMaxConns lets every one of the 32 concurrent writers in
// eventsourcingtest.Benchmark hold a connection, so the append benchmark
// measures the append lock rather than the pool. pgxpool's default is the
// number of CPUs, at least 4.
const benchmarkMaxConns = 64

func BenchmarkEventStore(b *testing.B) {
	eventsourcingtest.Benchmark(b, func(b *testing.B) eventsourcing.EventStore {
		return postgres.NewEventStore(newBenchmarkPool(b))
	})
}

func BenchmarkProjection(b *testing.B) {
	projectiontest.Benchmark(b, func(b *testing.B) (eventsourcing.EventStore, projection.Store, eventsourcing.EventHandler) {
		pool := newBenchmarkPool(b)
		if _, err := pool.Exec(b.Context(), benchmarkRowsSchema); err != nil {
			b.Fatalf("create benchmark_rows: %v", err)
		}
		return postgres.NewEventStore(pool), postgres.NewProjectionStore(pool), eventsourcing.NewEventHandlerFunc(writeBenchmarkRow)
	})
}

// benchmarkRowsSchema is the benchmark's read model: one row per stream,
// holding the version last applied. It starts empty for every group of
// benchmarks.
const benchmarkRowsSchema = `
CREATE TABLE IF NOT EXISTS benchmark_rows (
	stream_id TEXT PRIMARY KEY,
	version   BIGINT NOT NULL
);
TRUNCATE benchmark_rows`

// writeBenchmarkRow upserts the event's stream row in the projection's
// transaction, as a real projection writes its read model.
func writeBenchmarkRow(ctx context.Context, _ eventsourcing.Event) error {
	_, err := postgres.Tx(ctx).Exec(ctx,
		`INSERT INTO benchmark_rows (stream_id, version) VALUES ($1, $2)
		 ON CONFLICT (stream_id) DO UPDATE SET version = EXCLUDED.version`,
		eventsourcing.StreamIDFromContext(ctx), int64(eventsourcing.VersionFromContext(ctx)))
	return err
}

// newBenchmarkPool connects to the test database and empties the events
// table, so every group of benchmarks starts on an empty log. Projections
// are named with a fresh UUID, so the projection tables are left as they
// are.
func newBenchmarkPool(b *testing.B) *pgxpool.Pool {
	b.Helper()
	cfg, err := pgxpool.ParseConfig(testDSN)
	if err != nil {
		b.Fatalf("parse DSN: %v", err)
	}
	cfg.MaxConns = benchmarkMaxConns
	pool, err := pgxpool.NewWithConfig(b.Context(), cfg)
	if err != nil {
		b.Fatalf("connect pool: %v", err)
	}
	b.Cleanup(pool.Close)
	if _, err := pool.Exec(b.Context(), "TRUNCATE events RESTART IDENTITY"); err != nil {
		b.Fatalf("truncate: %v", err)
	}
	return pool
}
