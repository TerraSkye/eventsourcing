package projection_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v4"
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

func fastProbe(ps projection.Store, opts ...projection.ProbeOption) *projection.Probe {
	opts = append([]projection.ProbeOption{
		projection.WithPollBackoff(func() backoff.BackOff { return backoff.NewConstantBackOff(5 * time.Millisecond) }),
		projection.WithClaimTTL(time.Second),
	}, opts...)
	return projection.NewProbe(ps, "test", opts...)
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
	statuses, err := probe.Statuses(ctx)
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

// TestProbe_BackoffStopEndsTheWait covers WithPollBackoff bounding a wait
// whose context has no deadline.
func TestProbe_BackoffStopEndsTheWait(t *testing.T) {
	probe := fastProbe(memory.NewProjectionStore(), projection.WithPollBackoff(func() backoff.BackOff {
		return backoff.WithMaxRetries(&backoff.ZeroBackOff{}, 2)
	}))

	done := make(chan error, 1)
	go func() { done <- probe.WaitUntil(t.Context(), 1) }()
	select {
	case err := <-done:
		var nr *projection.NotReachedError
		if !errors.As(err, &nr) {
			t.Fatalf("WaitUntil = %v, want a *NotReachedError", err)
		}
		if nr.Err != nil {
			t.Errorf("Err = %v, want nil when the backoff stopped", nr.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitUntil did not give up when the backoff stopped")
	}
}

// TestRunner_PublishesStall covers that a stall reaches the store: while a
// segment retries, it still holds its claim, so without publishing other
// instances would keep seeing the phase of its last commit. A Probe waiting
// past the stall reports why, and sees it clear once the runner recovers.
func TestRunner_PublishesStall(t *testing.T) {
	es := &testStore{}
	ps := memory.NewProjectionStore()

	var failing atomic.Bool
	failing.Store(true)
	c := newCounter()
	c.fail = func(gv uint64) error {
		if gv == 2 && failing.Load() {
			return errors.New("boom")
		}
		return nil
	}
	save(t, es, 1)
	r := newSharedRunner(c, es, ps)
	defer runRunner(t, r)()

	probe := fastProbe(ps)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := probe.WaitUntil(ctx, 1); err != nil {
		t.Fatalf("WaitUntil(1): %v", err)
	}
	last := save(t, es, 1)

	for {
		statuses, err := probe.Statuses(ctx)
		if err != nil {
			t.Fatalf("Statuses: %v", err)
		}
		if len(statuses) == 1 && statuses[0].Phase == projection.Stalled {
			if !strings.Contains(statuses[0].LastError, "boom") {
				t.Fatalf("stored LastError = %q, want the handler's error", statuses[0].LastError)
			}
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("stored status never showed the stall; last %+v", statuses)
		}
		time.Sleep(5 * time.Millisecond)
	}

	short, cancelShort := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancelShort()
	err := probe.WaitUntil(short, last)
	var nr *projection.NotReachedError
	if !errors.As(err, &nr) {
		t.Fatalf("WaitUntil past the stall = %v, want a *NotReachedError", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "stalled") || !strings.Contains(msg, "boom") {
		t.Errorf("Error() = %q, want it to name the stall and its cause", msg)
	}

	failing.Store(false)
	if err := probe.WaitUntil(ctx, last); err != nil {
		t.Fatalf("WaitUntil after recovery: %v", err)
	}
	statuses, err := probe.Statuses(ctx)
	if err != nil {
		t.Fatalf("Statuses: %v", err)
	}
	if statuses[0].LastError != "" {
		t.Errorf("stored LastError = %q after recovery, want empty", statuses[0].LastError)
	}
}
