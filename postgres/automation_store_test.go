//go:build integration

package postgres_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/terraskye/eventsourcing/automation"
	"github.com/terraskye/eventsourcing/automation/automationtest"
	"github.com/terraskye/eventsourcing/postgres"
)

func TestAutomationStoreAcceptance(t *testing.T) {
	automationtest.StoreAcceptanceTest(t, postgres.NewAutomationStore(newPool(t)))
}

func TestAutomationStoreRenewLocksInKeyOrder(t *testing.T) {
	testAutomationStoreLockOrder(t, func(ctx context.Context, store *postgres.AutomationStore, held []automation.SegmentLease) ([]automation.SegmentLease, error) {
		return store.Renew(ctx, held, time.Minute)
	})
}

func TestAutomationStoreReleaseLocksInKeyOrder(t *testing.T) {
	testAutomationStoreLockOrder(t, func(ctx context.Context, store *postgres.AutomationStore, held []automation.SegmentLease) ([]automation.SegmentLease, error) {
		rel := make([]automation.ReleaseRequest, len(held))
		for i, l := range held {
			rel[i] = automation.ReleaseRequest{Lease: l}
		}
		return store.Release(ctx, rel)
	})
}

// testAutomationStoreLockOrder checks that update locks the rows of the
// leases it is given in key order, as Acquire does, whatever order they
// come in and whatever order the table holds them in. It acquires the
// segments a, b and c, rewrites their rows in the order c, b, a so that
// a scan of the table meets them in that order too, locks a's row in a
// transaction of its own, and calls update with the leases in the order
// c, b, a. A call that locks in key order waits on a before touching b
// and c, so their rows are still free while it waits; one that takes the
// rows as they come has locked c and b by then.
func testAutomationStoreLockOrder(t *testing.T, update func(context.Context, *postgres.AutomationStore, []automation.SegmentLease) ([]automation.SegmentLease, error)) {
	t.Helper()
	ctx := t.Context()
	pool := newPool(t)
	store := postgres.NewAutomationStore(pool)
	name := t.Name()

	res, err := store.Acquire(ctx, name, "owner", []automation.Segment{{Key: "a"}, {Key: "b"}, {Key: "c"}}, 3, time.Minute)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if len(res.Granted) != 3 {
		t.Fatalf("acquire granted %d segments, want 3", len(res.Granted))
	}
	held := slices.Clone(res.Granted)
	slices.SortFunc(held, func(x, y automation.SegmentLease) int { return strings.Compare(y.Key, x.Key) })
	for _, l := range held {
		if err := store.Defer(ctx, name, l.Key, 0); err != nil {
			t.Fatalf("rewrite %s: %v", l.Key, err)
		}
	}

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `SELECT FROM automation_segments WHERE name = $1 AND segment_key = 'a' FOR UPDATE`, name); err != nil {
		t.Fatalf("lock a: %v", err)
	}

	done := make(chan lockOrderResult, 1)
	go func() {
		lost, err := update(ctx, store, held)
		done <- lockOrderResult{lost, err}
	}()
	waitForLockWait(t, pool, done)

	// The update is waiting on a row lock. With a locked by holder and
	// nothing else, that is a: b and c must still be free.
	probe, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	_, err = probe.Exec(ctx, `SELECT FROM automation_segments WHERE name = $1 AND segment_key IN ('b', 'c') FOR UPDATE NOWAIT`, name)
	probe.Rollback(ctx)
	if err != nil {
		t.Fatalf("b or c is locked while the update waits on a, so the rows were not locked in key order: %v", err)
	}

	holder.Rollback(ctx)
	r := <-done
	if r.err != nil {
		t.Fatalf("update: %v", r.err)
	}
	if len(r.lost) != 0 {
		t.Errorf("update lost %v, want none", r.lost)
	}
}

type lockOrderResult struct {
	lost []automation.SegmentLease
	err  error
}

// waitForLockWait returns once a session's statement on
// automation_segments waits for a lock, or fails the test if none does
// within ten seconds or done delivers first. It polls outside any
// transaction: pg_stat_activity is a snapshot for the transaction it is
// first read in.
func waitForLockWait(t *testing.T, pool *pgxpool.Pool, done <-chan lockOrderResult) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		var waiting int
		err := pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND pid <> pg_backend_pid()
			  AND query LIKE '%automation_segments%'`).Scan(&waiting)
		if err != nil {
			t.Fatalf("wait for the lock wait: %v", err)
		}
		if waiting > 0 {
			return
		}
		select {
		case r := <-done:
			t.Fatalf("update returned before a was unlocked: lost %v, err %v", r.lost, r.err)
		case <-time.After(10 * time.Millisecond):
		}
	}
}
