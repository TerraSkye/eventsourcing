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
