//go:build integration || bench

package postgres_test

import (
	"context"
	"log"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	cqrs "github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/postgres"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// OrderCreated is the event type the event store and event bus tests
// append.
type OrderCreated struct {
	OrderID string
}

func (e *OrderCreated) AggregateID() string { return e.OrderID }
func (e *OrderCreated) EventType() string   { return "OrderCreated" }

// testDSN connects to the one container every test in the package shares.
// No test runs in parallel, so tests that need an empty events table get
// one from newPool.
var testDSN string

func TestMain(m *testing.M) {
	cqrs.RegisterEventByType(func() cqrs.Event { return &OrderCreated{} })

	ctx := context.Background()
	pgc, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("testdb"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
	)
	if err != nil {
		log.Fatalf("start postgres container: %v", err)
	}

	testDSN, err = pgc.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Fatalf("get connection string: %v", err)
	}

	pool, err := pgxpool.New(ctx, testDSN)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	// pgxpool.New is lazy; ping with retries to wait for the container to be
	// fully accepting connections (it can reset connections briefly after the
	// "ready" log line appears).
	for i := range 20 {
		if err = pool.Ping(ctx); err == nil {
			break
		}
		if i == 19 {
			log.Fatalf("ping postgres: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if _, err = pool.Exec(ctx, postgres.Schema); err != nil {
		log.Fatalf("apply schema: %v", err)
	}
	pool.Close()

	code := m.Run()
	_ = pgc.Terminate(ctx)
	os.Exit(code)
}

// newPool connects to the test database and empties the events and
// subscriptions tables. Projection tests use projections of their own
// name, so the projection tables are left as they are.
func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("connect pool: %v", err)
	}
	_, err = pool.Exec(ctx, "TRUNCATE events RESTART IDENTITY; TRUNCATE event_subscriptions")
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func newEnvelope(streamID string, event cqrs.Event) cqrs.Envelope {
	return cqrs.Envelope{
		EventID:    uuid.New(),
		StreamID:   streamID,
		Event:      event,
		OccurredAt: time.Now(),
		Metadata:   map[string]any{},
	}
}
