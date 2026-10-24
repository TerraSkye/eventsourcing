package projection_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/memory"
	"github.com/terraskye/eventsourcing/projection"
)

// newSharedRunner returns a runner of the counter projection "test" that
// publishes to ps, so a Probe on ps can follow it.
func newSharedRunner(c *counter, es *testStore, ps projection.Store) *projection.Runner {
	return projection.NewRunner(projection.Definition{
		Name:         "test",
		Handler:      eventsourcing.NewEventHandlerFunc(c.Handle),
		EventTypes:   counterEvents,
		PollInterval: 10 * time.Millisecond,
		ClaimTTL:     time.Second,
	}, es, ps)
}

func fastProbe(ps projection.Store) *projection.Probe {
	return projection.NewProbe(projection.ProbeDefinition{
		Name:         "test",
		PollInterval: 5 * time.Millisecond,
		ClaimTTL:     time.Second,
	}, ps)
}

// fakeStatuses is a Store whose one segment's position and read error a
// test sets directly, and which counts its reads. A Probe only calls
// LoadStatuses; the embedded nil Store panics on anything else.
type fakeStatuses struct {
	projection.Store

	mu     sync.Mutex
	status projection.Status
	err    error
	reads  atomic.Int64
}

func newFakeStatuses(position uint64) *fakeStatuses {
	return &fakeStatuses{status: projection.Status{Phase: projection.CatchingUp, Position: position, Owner: "worker"}}
}

func (f *fakeStatuses) LoadStatuses(context.Context, string) ([]projection.Status, error) {
	f.reads.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return []projection.Status{f.status}, nil
}

func (f *fakeStatuses) set(fn func(f *fakeStatuses)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// TestProbe_FollowsRunnerInAnotherProcess covers waiting on a projection
// with only its store and name, as a process that serves the read model
// but does not run the projection does.
func TestProbe_FollowsRunnerInAnotherProcess(t *testing.T) {
	es := &testStore{}
	ps := memory.NewProjectionStore()
	defer runRunner(t, newSharedRunner(newCounter(), es, ps))()

	probe := fastProbe(ps)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	last := save(t, es, 3)
	if err := probe.WaitUntil(ctx, last); err != nil {
		t.Fatalf("WaitUntil(%d): %v", last, err)
	}
	statuses, err := ps.LoadStatuses(ctx, "test")
	if err != nil {
		t.Fatalf("Statuses: %v", err)
	}
	if len(statuses) != 1 || statuses[0].Position < last {
		t.Fatalf("Statuses() = %+v, want one segment at or past %d", statuses, last)
	}
	for {
		live, err := probe.IsLive(ctx)
		if err != nil {
			t.Fatalf("IsLive: %v", err)
		}
		if live {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("probe never saw the projection live")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestProbe_UnstartedProjectionHasReachedNothing covers a projection no
// runner has declared yet: an empty status list must not pass for "every
// segment reached the position".
func TestProbe_UnstartedProjectionHasReachedNothing(t *testing.T) {
	probe := fastProbe(memory.NewProjectionStore())

	live, err := probe.IsLive(t.Context())
	if err != nil || live {
		t.Fatalf("IsLive() = %v, %v; want false, nil", live, err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	err = probe.WaitUntil(ctx, 1)
	var nr *projection.NotReachedError
	if !errors.As(err, &nr) {
		t.Fatalf("WaitUntil = %v, want a *NotReachedError", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("WaitUntil = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if len(nr.Statuses) != 0 {
		t.Errorf("Statuses = %+v, want none", nr.Statuses)
	}
	if !strings.Contains(err.Error(), "no status published") {
		t.Errorf("Error() = %q, want it to say no status was published", err)
	}
}

// TestProbe_WaitsShareOneRead covers that concurrent waits share the
// poller's reads, so store load follows the poll interval rather than the
// number of waiters, and that the poller stops with the last wait.
func TestProbe_WaitsShareOneRead(t *testing.T) {
	store := newFakeStatuses(0)
	probe := projection.NewProbe(projection.ProbeDefinition{Name: "test", PollInterval: 20 * time.Millisecond, ClaimTTL: time.Second}, store)

	const waiters = 50
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	var wg sync.WaitGroup
	for range waiters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = probe.WaitUntil(ctx, 10)
		}()
	}
	wg.Wait()

	// 200ms at 20ms is about 11 reads; a read per waiter per interval
	// would be about 500.
	reads := store.reads.Load()
	if reads > 20 {
		t.Errorf("%d waiters made %d reads in 200ms at a 20ms interval, want them shared", waiters, reads)
	}

	time.Sleep(60 * time.Millisecond)
	if after := store.reads.Load(); after != reads {
		t.Errorf("reads went from %d to %d with no wait in progress, want the poller stopped", reads, after)
	}
}

// TestProbe_StoreErrorDoesNotEndTheWait covers that a failing store delays
// waiters instead of failing them, and that a wait the context ends
// reports the store's error next to the context's.
func TestProbe_StoreErrorDoesNotEndTheWait(t *testing.T) {
	boom := errors.New("connection reset")
	store := newFakeStatuses(1)
	store.status.Phase = projection.Stalled
	store.status.LastError = "handler failed"
	probe := fastProbe(store)

	// A wait the context ends while the store fails reports both errors,
	// and the statuses of the last read that succeeded.
	failing, cancelFailing := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancelFailing()
	go func() {
		time.Sleep(30 * time.Millisecond)
		store.set(func(f *fakeStatuses) { f.err = boom })
	}()
	err := probe.WaitUntil(failing, 5)
	var nr *projection.NotReachedError
	if !errors.As(err, &nr) {
		t.Fatalf("WaitUntil = %v, want a *NotReachedError", err)
	}
	if !errors.Is(err, boom) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("WaitUntil = %v, want it to match both the store's and the context's error", err)
	}
	if behind := nr.Behind(); behind == nil || behind.Phase != projection.Stalled {
		t.Errorf("Behind() = %+v, want the stalled segment from the last good read", behind)
	}

	// A wait that outlasts the failure returns once the position is reached.
	done := make(chan error, 1)
	go func() { done <- probe.WaitUntil(t.Context(), 5) }()
	time.Sleep(30 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("WaitUntil returned %v while the store was failing, want it to keep waiting", err)
	default:
	}
	store.set(func(f *fakeStatuses) {
		f.err = nil
		f.status = projection.Status{Phase: projection.Live, Position: 5, Owner: "worker"}
	})
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitUntil after the store recovered: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitUntil did not return after the store recovered")
	}
}

// TestProbe_CancelledWaitLeavesOthersWaiting covers that one waiter's
// context only ends that wait, as the shared read runs under the poller's
// own context.
func TestProbe_CancelledWaitLeavesOthersWaiting(t *testing.T) {
	store := newFakeStatuses(0)
	probe := fastProbe(store)

	short, cancelShort := context.WithCancel(t.Context())
	shortDone := make(chan error, 1)
	go func() { shortDone <- probe.WaitUntil(short, 3) }()
	longDone := make(chan error, 1)
	go func() { longDone <- probe.WaitUntil(t.Context(), 3) }()

	time.Sleep(20 * time.Millisecond)
	cancelShort()
	if err := <-shortDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled WaitUntil = %v, want context.Canceled", err)
	}

	store.set(func(f *fakeStatuses) { f.status.Position = 3 })
	select {
	case err := <-longDone:
		if err != nil {
			t.Fatalf("other WaitUntil = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("other WaitUntil did not return after the position was reached")
	}
}

// TestProbe_Instances covers a Probe listing a projection's instances: a
// runner on standby, which holds no segment, is listed beside the one that
// holds it, and the Probe does not count as an instance itself.
func TestProbe_Instances(t *testing.T) {
	es, ps := &testStore{}, memory.NewProjectionStore()
	save(t, es, 1)
	a := newSpreadRunner(es, ps, "a", 1, 0)
	defer runRunner(t, a)()
	waitHeld(t, ps, map[string]int{"a": 1}, 2*time.Second)
	b := newSpreadRunner(es, ps, "b", 1, 0)
	defer runRunner(t, b)()

	probe := projection.NewProbe(projection.ProbeDefinition{Name: "spread"}, ps)
	deadline := time.Now().Add(2 * time.Second)
	for {
		instances, err := probe.Instances(t.Context())
		if err != nil {
			t.Fatalf("Instances: %v", err)
		}
		held := map[string]int{}
		for _, m := range instances {
			held[m.Owner] = m.Segments
		}
		if len(held) == 2 && held[a.Owner()] == 1 && held[b.Owner()] == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Instances = %+v, want %s holding the segment and %s on standby", instances, a.Owner(), b.Owner())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestNewProbe_Panics covers the wiring mistakes NewProbe refuses, as
// NewRunner does its own.
func TestNewProbe_Panics(t *testing.T) {
	tests := []struct {
		name  string
		def   projection.ProbeDefinition
		store projection.Store
	}{
		{name: "empty name", def: projection.ProbeDefinition{}, store: memory.NewProjectionStore()},
		{name: "nil store", def: projection.ProbeDefinition{Name: "x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("NewProbe did not panic")
				}
			}()
			projection.NewProbe(tt.def, tt.store)
		})
	}
}
