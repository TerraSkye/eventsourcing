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
