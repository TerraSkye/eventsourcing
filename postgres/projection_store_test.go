//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing/postgres"
	"github.com/terraskye/eventsourcing/projection"
	"github.com/terraskye/eventsourcing/projection/projectiontest"
)

func TestStoreAcceptance(t *testing.T) {
	projectiontest.StoreAcceptanceTest(t, postgres.NewProjectionStore(newPool(t)))
}

// TestCommit_BatchLongerThanTTLKeepsClaim checks that a Claim made while a
// Commit runs longer than the claim's ttl waits for the batch, and is
// refused once it commits: the batch's heartbeat is the time of its write,
// not the start of its transaction.
func TestCommit_BatchLongerThanTTLKeepsClaim(t *testing.T) {
	ctx := t.Context()
	store := postgres.NewProjectionStore(newPool(t))
	name := "long-batch-" + uuid.NewString()
	const ttl = 300 * time.Millisecond
	a := projection.Lease{Name: name, Segment: 0, Owner: "a"}
	b := projection.Lease{Name: name, Segment: 0, Owner: "b"}

	if err := store.Declare(ctx, name, 1, 0); err != nil {
		t.Fatalf("Declare: %v", err)
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
