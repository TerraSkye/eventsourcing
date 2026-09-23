package file

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
)

// TestDispatch_SimultaneousCallsEachWriteTheirOwnFile pins that two Dispatch
// calls never write the same filename.
//
// Dispatch used to name each file from time.Now().UnixNano() alone, so calls
// landing on the same nanosecond built the same name and the second rename
// replaced the first event's file — destroying an event the subscriber had
// not read yet, with nothing left on disk or in Errors() to notice.
//
// The subscriber's handler blocks for the duration, so nothing is consumed
// and the directory is a faithful record of what Dispatch wrote. Rounds of
// goroutines are released together on a barrier, which is what makes same
// nanosecond collisions likely enough to be worth asserting on; counting
// files rather than deliveries keeps the assertion about the write side
// alone.
func TestDispatch_SimultaneousCallsEachWriteTheirOwnFile(t *testing.T) {
	if testing.Short() {
		t.Skip("dispatches several thousand events")
	}

	root := t.TempDir()
	bus, err := NewFileEventBus(root)
	if err != nil {
		t.Fatalf("NewFileEventBus: %v", err)
	}
	defer bus.Close()

	// Block delivery so no file is ever removed: the subscriber's goroutine
	// parks in the first handler call and the rest stay on disk.
	release := make(chan struct{})
	defer close(release)

	if err := bus.Subscribe(context.Background(), "orders",
		eventsourcing.NewEventHandlerFunc(func(ctx context.Context, ev eventsourcing.Event) error {
			<-release
			return nil
		})); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	const rounds = 400
	const perRound = 16
	total := rounds * perRound

	for r := 0; r < rounds; r++ {
		start := make(chan struct{})
		var wg sync.WaitGroup
		for g := 0; g < perRound; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				<-start // released together, to land on the same nanosecond
				id := fmt.Sprintf("order-%d-%d", r, g)
				if err := bus.Dispatch(&eventsourcing.Envelope{
					EventID:    uuid.New(),
					StreamID:   id,
					Event:      orderPlaced{ID: id, Total: g},
					OccurredAt: time.Now().UTC(),
				}); err != nil {
					t.Errorf("Dispatch: %v", err)
				}
			}(g)
		}
		close(start)
		wg.Wait()
	}

	entries, err := os.ReadDir(filepath.Join(root, "orders"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	written := 0
	for _, e := range entries {
		if !e.IsDir() {
			written++
		}
	}

	// At most one file can be missing: the one the parked handler is holding,
	// which processFile removes only after the handler returns — it has not.
	if written < total-1 {
		t.Fatalf("dispatched %d events, only %d files exist: %d were overwritten by a "+
			"colliding filename and no longer exist anywhere", total, written, total-written)
	}
}
