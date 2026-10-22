package projection_test

import (
	"context"
	"errors"
	"sync"
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
