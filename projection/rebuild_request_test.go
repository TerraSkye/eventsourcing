package projection_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/memory"
	"github.com/terraskye/eventsourcing/projection"
)

// newInstance returns a runner of the counter projection "test" on ps, as
// one instance of a program that may run several. Without reset, the
// projection has no Reset.
func newInstance(c *counter, es *testStore, ps projection.Store, owner string, reset bool) *projection.Runner {
	p := projection.Definition{
		Name:         "test",
		Handler:      eventsourcing.NewEventHandlerFunc(c.Handle),
		EventTypes:   counterEvents,
		Owner:        owner,
		PollInterval: 10 * time.Millisecond,
		ClaimTTL:     time.Second,
	}
	if reset {
		p.Reset, p.Version = c.Reset, 1
	}
	return projection.NewRunner(p, es, ps)
}

func resets(c *counter) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.resets
}

// waitRebuilt waits until the store marks rebuild request n done.
func waitRebuilt(t *testing.T, ps projection.Store, n uint64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, done, err := ps.Rebuilds(t.Context(), "test")
		if err != nil {
			t.Fatalf("Rebuilds: %v", err)
		}
		if done >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("rebuild request %d not served within 3s (done %d)", n, done)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRunner_RebuildBeforeDeclare(t *testing.T) {
	r := newRunner(newCounter(), &testStore{}, 1)
	if err := r.Rebuild(t.Context()); !errors.Is(err, projection.ErrUnknownProjection) {
		t.Fatalf("Rebuild before any runner declared the projection = %v, want ErrUnknownProjection", err)
	}
}

// TestRunner_RebuildFromAnotherInstance covers calling Rebuild on an
// instance that does not hold segment 0: the instance that does rebuilds.
func TestRunner_RebuildFromAnotherInstance(t *testing.T) {
	es, ps := &testStore{}, memory.NewProjectionStore()
	last := save(t, es, 3)

	holder, standby := newCounter(), newCounter()
	a := newInstance(holder, es, ps, "a", true)
	defer runRunner(t, a)()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := a.WaitUntil(ctx, last); err != nil {
		t.Fatalf("WaitUntil: %v", err)
	}
	b := newInstance(standby, es, ps, "b", true)
	defer runRunner(t, b)()

	if err := b.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild on the standby instance = %v, want nil", err)
	}
	if n, m := resets(holder), resets(standby); n != 1 || m != 0 {
		t.Errorf("Reset ran %d times on the holder and %d on the standby, want 1 and 0", n, m)
	}
	if err := a.WaitUntil(ctx, last); err != nil {
		t.Fatalf("WaitUntil after the rebuild: %v", err)
	}
}

// TestStore_RebuildRequestOutlivesTheRunner covers a request made while no
// runner holds the projection, as from a web process or after a crash: the
// next runner to claim segment 0 serves it.
func TestStore_RebuildRequestOutlivesTheRunner(t *testing.T) {
	es, ps := &testStore{}, memory.NewProjectionStore()
	last := save(t, es, 3)

	first := newCounter()
	stop := runRunner(t, newInstance(first, es, ps, "a", true))
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	waitPosition(ctx, t, ps, last)
	stop()

	n, err := ps.RequestRebuild(ctx, "test")
	if err != nil {
		t.Fatalf("RequestRebuild: %v", err)
	}
	statuses, _ := ps.LoadStatuses(ctx, "test")
	if len(statuses) == 0 || !statuses[0].RebuildRequested {
		t.Fatalf("statuses = %+v, want the request shown as pending", statuses)
	}

	next := newCounter()
	defer runRunner(t, newInstance(next, es, ps, "b", true))()
	waitRebuilt(t, ps, n)
	if got := resets(next); got != 1 {
		t.Errorf("Reset ran %d times on the next runner, want once", got)
	}
}

// TestStore_RebuildRequestsShareARebuild covers that requests made before
// a rebuild runs are all served by it.
func TestStore_RebuildRequestsShareARebuild(t *testing.T) {
	es, ps := &testStore{}, memory.NewProjectionStore()
	last := save(t, es, 3)

	stop := runRunner(t, newInstance(newCounter(), es, ps, "a", true))
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	waitPosition(ctx, t, ps, last)
	stop()

	var n uint64
	for range 3 {
		var err error
		if n, err = ps.RequestRebuild(ctx, "test"); err != nil {
			t.Fatalf("RequestRebuild: %v", err)
		}
	}
	c := newCounter()
	defer runRunner(t, newInstance(c, es, ps, "b", true))()
	waitRebuilt(t, ps, n)
	if got := resets(c); got != 1 {
		t.Errorf("Reset ran %d times for 3 requests, want once", got)
	}
}

// TestStore_RebuildWithoutResetStaysPending covers a request for a
// projection with no Reset: the runner cannot serve it, so it stays
// pending and visible, and the projection keeps running.
func TestStore_RebuildWithoutResetStaysPending(t *testing.T) {
	es, ps := &testStore{}, memory.NewProjectionStore()
	last := save(t, es, 3)

	defer runRunner(t, newInstance(newCounter(), es, ps, "a", false))()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	waitPosition(ctx, t, ps, last)

	if _, err := ps.RequestRebuild(ctx, "test"); err != nil {
		t.Fatalf("RequestRebuild: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // ten of the runner's checks

	if requested, done, _ := ps.Rebuilds(ctx, "test"); requested != 1 || done != 0 {
		t.Errorf("Rebuilds = %d, %d; want 1, 0: nothing can serve it", requested, done)
	}
	more := save(t, es, 2)
	waitPosition(ctx, t, ps, more)
}

// waitPosition waits until segment 0 of "test" has processed position.
func waitPosition(ctx context.Context, t *testing.T, ps projection.Store, position uint64) {
	t.Helper()
	if err := projection.NewProbe(ps, "test", projection.WithPollInterval(5*time.Millisecond)).WaitUntil(ctx, position); err != nil {
		t.Fatalf("waiting for position %d: %v", position, err)
	}
}
