package projection_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/memory"
	"github.com/terraskye/eventsourcing/projection"
)

// TestRunner_StallBackoffGrows covers a handler that keeps failing under
// StallOnError: the waits between retries grow from 0.5s towards 30s,
// although every retry claims the segment again.
func TestRunner_StallBackoffGrows(t *testing.T) {
	tests := []struct {
		name     string
		segments int
	}{
		{name: "single segment", segments: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			es := &testStore{}
			ps := memory.NewProjectionStore()
			if _, err := es.Save(t.Context(), []eventsourcing.Envelope{
				{EventID: uuid.New(), StreamID: "s", Event: &taskCreated{ID: "s"}, Version: 1},
			}, eventsourcing.Any{}); err != nil {
				t.Fatalf("Save: %v", err)
			}
			var mu sync.Mutex
			var attempts []time.Time
			h := eventsourcing.NewEventHandlerFunc(func(context.Context, eventsourcing.Event) error {
				mu.Lock()
				defer mu.Unlock()
				now := time.Now()
				// The batch and its one-by-one retry are one attempt.
				if n := len(attempts); n == 0 || now.Sub(attempts[n-1]) > 100*time.Millisecond {
					attempts = append(attempts, now)
				}
				return errors.New("always fails")
			})
			r := projection.NewRunner(projection.Definition{
				Name:         "p",
				Owner:        "a",
				Handler:      h,
				EventTypes:   counterEvents,
				PollInterval: 10 * time.Millisecond,
				ClaimTTL:     time.Minute,
			}, es, ps)
			defer runRunner(t, r)()

			deadline := time.After(10 * time.Second)
			for {
				mu.Lock()
				n := len(attempts)
				mu.Unlock()
				if n >= 4 {
					break
				}
				select {
				case <-deadline:
					t.Fatalf("only %d attempts in 10s", n)
				case <-time.After(10 * time.Millisecond):
				}
			}
			mu.Lock()
			defer mu.Unlock()
			var gaps []time.Duration
			for i := 1; i < 4; i++ {
				gaps = append(gaps, attempts[i].Sub(attempts[i-1]))
			}
			// 0.5s, 1s, 2s with ±50% jitter: the third wait is at least 1s,
			// while a 0.5s wait that never grows is at most 0.75s.
			if gaps[2] < 900*time.Millisecond {
				t.Fatalf("stall backoff does not grow: waits between attempts %v", gaps)
			}
		})
	}
}

// claimErrStore fails the first fails calls to Claim, as a store with a
// transient connection error would.
type claimErrStore struct {
	projection.Store
	mu    sync.Mutex
	fails int
}

func (s *claimErrStore) Claim(ctx context.Context, l projection.Lease, ttl time.Duration) (projection.Lease, projection.Checkpoint, bool, error) {
	s.mu.Lock()
	fail := s.fails > 0
	if fail {
		s.fails--
	}
	s.mu.Unlock()
	if fail {
		return projection.Lease{}, projection.Checkpoint{}, false, errors.New("connection reset")
	}
	return s.Store.Claim(ctx, l, ttl)
}

// TestRunner_ClaimErrorLeavesOwnerToStore covers a Runner whose Claim fails
// with a store error while another instance holds the segment: it does not
// report itself as the owner, so IsLive and WaitUntil ask the store, where
// the real owner's progress is.
func TestRunner_ClaimErrorLeavesOwnerToStore(t *testing.T) {
	newDef := func(owner string, segments int) projection.Definition {
		d := projection.Definition{
			Name:         "test",
			Handler:      eventsourcing.NewEventHandlerFunc(newCounter().Handle),
			EventTypes:   counterEvents,
			Owner:        owner,
			PollInterval: 10 * time.Millisecond,
			ClaimTTL:     time.Second,
		}
		if segments > 1 {
			d.Segments, d.SegmentKey = segments, projection.ByStreamID
		}
		return d
	}

	tests := []struct {
		name     string
		segments int
	}{
		{name: "one segment", segments: 1},
		{name: "two segments", segments: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			es := &testStore{}
			ps := memory.NewProjectionStore()

			// b owns every segment and processes the log.
			b := projection.NewRunner(newDef("b", tt.segments), es, ps)
			defer runRunner(t, b)()
			pos := save(t, es, 3)
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			if err := b.WaitUntil(ctx, pos); err != nil {
				t.Fatalf("b.WaitUntil(%d): %v", pos, err)
			}

			// a's first Claim on each segment fails with a store error.
			as := &claimErrStore{Store: ps, fails: tt.segments}
			a := projection.NewRunner(newDef("a", tt.segments), es, as)
			defer runRunner(t, a)()
			for a.Status().Phase != projection.Stalled {
				if ctx.Err() != nil {
					t.Fatal("a never stalled on its failing Claim")
				}
				time.Sleep(time.Millisecond)
			}

			if st := a.Status(); st.Owner == "a" {
				t.Errorf("a.Status().Owner = %q after a Claim error, while b holds every segment; Status() = %+v", st.Owner, st)
			}
			if tt.segments == 1 {
				live, err := a.IsLive(t.Context())
				if err != nil || !live {
					t.Errorf("a.IsLive() = %v, %v; want true, nil: b owns the segment and is live", live, err)
				}
			}
			// 200ms is 20 PollIntervals, well before a's retry backoff ends.
			wctx, wcancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer wcancel()
			if err := a.WaitUntil(wctx, pos); err != nil {
				t.Errorf("a.WaitUntil(%d) = %v; b already processed it", pos, err)
			}
		})
	}
}

// TestRunner_NotOwnerAfterRunReturns covers a Runner whose Run has returned,
// and so released every claim: IsLive and WaitUntil answer from the store
// rather than from its last in-memory status.
func TestRunner_NotOwnerAfterRunReturns(t *testing.T) {
	newDef := func(owner string) projection.Definition {
		return projection.Definition{
			Name:         "test",
			Handler:      eventsourcing.NewEventHandlerFunc(newCounter().Handle),
			EventTypes:   counterEvents,
			Owner:        owner,
			PollInterval: 10 * time.Millisecond,
			ClaimTTL:     time.Second,
		}
	}
	// startAndStop runs a until it is live, then stops it.
	startAndStop := func(t *testing.T, a *projection.Runner) {
		stop := runRunner(t, a)
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		for {
			if live, _ := a.IsLive(ctx); live {
				break
			}
			if ctx.Err() != nil {
				t.Fatal("runner never went live")
			}
			time.Sleep(10 * time.Millisecond)
		}
		stop() // Run has returned and released the claim
	}

	tests := []struct {
		name  string
		check func(t *testing.T, es *testStore, ps *memory.ProjectionStore, a *projection.Runner)
	}{
		{
			name: "IsLive with an unprocessed event and no owner",
			check: func(t *testing.T, es *testStore, ps *memory.ProjectionStore, a *projection.Runner) {
				pos := save(t, es, 1)
				live, err := a.IsLive(t.Context())
				if err != nil {
					t.Fatalf("IsLive: %v", err)
				}
				if live {
					t.Errorf("IsLive() = true after Run returned, with event %d unprocessed by anyone; Status() = %+v", pos, a.Status())
				}
			},
		},
		{
			name: "WaitUntil for a position another instance reached",
			check: func(t *testing.T, es *testStore, ps *memory.ProjectionStore, a *projection.Runner) {
				pos := save(t, es, 1)
				b := projection.NewRunner(newDef("b"), es, ps)
				defer runRunner(t, b)()
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				defer cancel()
				if err := b.WaitUntil(ctx, pos); err != nil {
					t.Fatalf("b.WaitUntil(%d): %v", pos, err)
				}
				ctx, cancel = context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				if err := a.WaitUntil(ctx, pos); err != nil {
					t.Errorf("a.WaitUntil(%d) = %v after b processed it; a.Status() = %+v", pos, err, a.Status())
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			es := &testStore{}
			ps := memory.NewProjectionStore()
			a := projection.NewRunner(newDef("a"), es, ps)
			startAndStop(t, a)
			tt.check(t, es, ps, a)
		})
	}
}

type oneByOneUnhandled struct{}

func (oneByOneUnhandled) AggregateID() string { return "other" }
func (oneByOneUnhandled) EventType() string   { return "projection_test.OneByOneUnhandled" }

// TestRunner_OneByOneRetryCommitsScannedPosition covers a batch whose
// handler fails once and succeeds when the runner retries it event by
// event, where the read also scanned events the projection does not handle
// past the last handled one. The store's checkpoint moves to the end of the
// read too, so a wait on the store sees the scanned position.
func TestRunner_OneByOneRetryCommitsScannedPosition(t *testing.T) {
	tests := []struct {
		name     string
		segments int
		envs     []eventsourcing.Envelope
	}{
		{
			// Position 2 is filtered out by EventTypes.
			name:     "unhandled event type after the failing event",
			segments: 1,
			envs: []eventsourcing.Envelope{
				{EventID: uuid.New(), StreamID: "s", Event: &taskCreated{ID: "s"}, Version: 1},
				{EventID: uuid.New(), StreamID: "other", Event: oneByOneUnhandled{}, Version: 1},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			es := &testStore{}
			ps := memory.NewProjectionStore()
			res, err := es.Save(t.Context(), tt.envs, eventsourcing.Any{})
			if err != nil {
				t.Fatalf("Save: %v", err)
			}
			last := res.GlobalPosition

			// The first batch fails; the event-by-event retry succeeds.
			var calls atomic.Int32
			h := eventsourcing.NewEventHandlerFunc(func(context.Context, eventsourcing.Event) error {
				if calls.Add(1) == 1 {
					return errors.New("transient")
				}
				return nil
			})
			r := projection.NewRunner(projection.Definition{
				Name:         "p",
				Owner:        "a",
				Handler:      h,
				EventTypes:   counterEvents,
				PollInterval: 10 * time.Millisecond,
				ClaimTTL:     time.Second,
			}, es, ps)
			defer runRunner(t, r)()

			// The runner itself gets past the whole read.
			deadline := time.After(2 * time.Second)
			for r.Status().Position < last {
				select {
				case <-deadline:
					t.Fatalf("runner never reached %d in memory: %+v", last, r.Status())
				case <-time.After(5 * time.Millisecond):
				}
			}

			// Every event up to last is processed, so a wait on the store
			// must return within a few poll intervals.
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			probe := projection.NewProbe(ps, "p", projection.WithPollInterval(5*time.Millisecond))
			if err := probe.WaitUntil(ctx, last); err != nil {
				statuses, _ := ps.LoadStatuses(t.Context(), "p")
				t.Fatalf("Probe.WaitUntil(%d) = %v\nin memory: %+v\nin store: %+v", last, err, r.Status(), statuses)
			}
		})
	}
}
