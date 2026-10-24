package projection_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/memory"
	"github.com/terraskye/eventsourcing/projection"
)

// TestRunner_WatchCalledWhenPhaseGoesLiveWithoutEvents covers Runner.Watch's
// promise that fn is called "on every change of Phase": a runner whose
// segment goes from Starting to Live without a batch of events must call
// its watchers.
func TestRunner_WatchCalledWhenPhaseGoesLiveWithoutEvents(t *testing.T) {
	def := projection.Definition{
		Name:         "watch",
		Handler:      eventsourcing.NewEventHandlerFunc(func(context.Context, eventsourcing.Event) error { return nil }),
		EventTypes:   []string{"projection_test.TaskCreated"},
		PollInterval: 10 * time.Second,
	}
	// runUntilLive runs r until it reports Live and returns the phase seen
	// by each watcher call.
	runUntilLive := func(t *testing.T, r *projection.Runner) []projection.Phase {
		t.Helper()
		var mu sync.Mutex
		var phases []projection.Phase
		r.Watch(func() {
			mu.Lock()
			phases = append(phases, r.Status().Phase)
			mu.Unlock()
		})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { _ = r.Run(ctx); close(done) }()
		defer func() { cancel(); <-done }()
		deadline := time.Now().Add(2 * time.Second)
		for r.Status().Phase != projection.Live {
			if time.Now().After(deadline) {
				t.Fatalf("never live: %+v", r.Status())
			}
			time.Sleep(5 * time.Millisecond)
		}
		// Give a late notification time to arrive.
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		return append([]projection.Phase(nil), phases...)
	}

	tests := []struct {
		name  string
		setup func(t *testing.T, es *memory.EventStore, store projection.Store)
	}{
		{
			name:  "empty log",
			setup: func(*testing.T, *memory.EventStore, projection.Store) {},
		},
		{
			name: "restart with checkpoint at head",
			setup: func(t *testing.T, es *memory.EventStore, store projection.Store) {
				if _, err := es.Save(t.Context(), []eventsourcing.Envelope{{
					EventID: uuid.New(), StreamID: "s-1", Event: &taskCreated{ID: "t"}, Version: 1,
				}}, eventsourcing.Any{}); err != nil {
					t.Fatal(err)
				}
				// A first instance catches up and stops.
				runUntilLive(t, projection.NewRunner(def, es, store))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			es := memory.NewEventStore(16)
			store := memory.NewProjectionStore()
			tt.setup(t, es, store)

			phases := runUntilLive(t, projection.NewRunner(def, es, store))
			for _, p := range phases {
				if p == projection.Live {
					return
				}
			}
			t.Errorf("phase changed from Starting to Live, but no watcher call saw it: watcher calls saw %v", phases)
		})
	}
}

// TestStatus_IsLiveUsesClaimTTL covers a status that carries its claim's
// ttl: IsLive judges its age by that ttl rather than the reader's own.
func TestStatus_IsLiveUsesClaimTTL(t *testing.T) {
	live := projection.Status{Phase: projection.Live, Owner: "a", Age: 10 * time.Second}
	tests := []struct {
		name     string
		claimTTL time.Duration // the status's own
		readTTL  time.Duration // the reader's
		want     bool
	}{
		{name: "reader's ttl when the status has none", readTTL: 15 * time.Second, want: true},
		{name: "reader's ttl too short when the status has none", readTTL: 5 * time.Second, want: false},
		{name: "claim's longer ttl over a shorter reader's", claimTTL: 15 * time.Second, readTTL: 5 * time.Second, want: true},
		{name: "claim's shorter ttl over a longer reader's", claimTTL: 5 * time.Second, readTTL: 15 * time.Second, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := live
			st.ClaimTTL = tt.claimTTL
			if got := st.IsLive(tt.readTTL); got != tt.want {
				t.Errorf("IsLive(%s) with ClaimTTL %s = %v, want %v", tt.readTTL, tt.claimTTL, got, tt.want)
			}
		})
	}
}
