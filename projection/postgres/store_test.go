//go:build integration

package postgres_test

import (
	"context"
	"log"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/terraskye/eventsourcing/projection"
	"github.com/terraskye/eventsourcing/projection/postgres"
	"github.com/terraskye/eventsourcing/projection/projectiontest"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

var testDSN string

func TestMain(m *testing.M) {
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
	// fully accepting connections.
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

func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), testDSN)
	if err != nil {
		t.Fatalf("connect pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestStoreAcceptance(t *testing.T) {
	projectiontest.StoreAcceptanceTest(t, postgres.NewStore(newPool(t)))
}

// TestCommit_BatchLongerThanTTLKeepsClaim checks that a Claim made while a
// Commit runs longer than the claim's ttl waits for the batch, and is
// refused once it commits: the batch's heartbeat is the time of its write,
// not the start of its transaction.
func TestCommit_BatchLongerThanTTLKeepsClaim(t *testing.T) {
	ctx := t.Context()
	store := postgres.NewStore(newPool(t))
	name := "long-batch-" + uuid.NewString()
	const ttl = 300 * time.Millisecond
	a := projection.Lease{Name: name, Segment: 0, Owner: "a"}
	b := projection.Lease{Name: name, Segment: 0, Owner: "b"}

	if err := store.Init(ctx, name, 1, 0); err != nil {
		t.Fatalf("Init: %v", err)
	}
	a, _, ok, err := store.Claim(ctx, a, ttl)
	if err != nil || !ok {
		t.Fatalf("Claim(a) = %v, %v; want true, nil", ok, err)
	}

	inBatch := make(chan struct{})
	claimed := make(chan bool, 1)
	go func() {
		<-inBatch
		time.Sleep(2 * ttl) // the claim has expired by the time this runs
		_, _, ok, err := store.Claim(ctx, b, ttl)
		if err != nil {
			t.Errorf("Claim(b): %v", err)
		}
		claimed <- ok
	}()

	cp := projection.Checkpoint{Position: 1}
	st := projection.Status{Phase: projection.Live, Position: 1, Owner: "a"}
	err = store.Commit(ctx, a, cp, st, func(context.Context) error {
		close(inBatch)
		time.Sleep(4 * ttl)
		return nil
	})
	if err != nil {
		t.Fatalf("Commit of the long batch: %v", err)
	}

	if <-claimed {
		t.Fatal("Claim(b) was granted right after a long batch committed, want it refused")
	}
	if err := store.Commit(ctx, a, projection.Checkpoint{Position: 2}, projection.Status{Phase: projection.Live, Position: 2, Owner: "a"},
		func(context.Context) error { return nil }); err != nil {
		t.Fatalf("next Commit by the owner: %v", err)
	}
}
