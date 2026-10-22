package projection_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/memory"
	"github.com/terraskye/eventsourcing/projection"
)

// TestRunner_RebuildMidReplayKeepsReplayUntil covers that a rebuild started
// before a segment has passed its ReplayUntil keeps every event that was in
// the log at the first start counted as replayed.
func TestRunner_RebuildMidReplayKeepsReplayUntil(t *testing.T) {
	es := &testStore{}
	last := save(t, es, 10) // all ten are in the log before the first start
	c := newCounter()
	// Hold the first replay at position 4 by failing event 5 until the
	// rebuild is requested.
	var poison atomic.Bool
	poison.Store(true)
	c.fail = func(gv uint64) error {
		if gv == 5 && poison.Load() {
			return errors.New("poison")
		}
		return nil
	}
	r := projection.NewRunner(projection.Definition{
		Name:         "test",
		Handler:      eventsourcing.NewEventHandlerFunc(c.Handle),
		EventTypes:   counterEvents,
		Reset:        c.Reset,
		Version:      1,
		BatchSize:    2,
		PollInterval: 10 * time.Millisecond,
		ClaimTTL:     time.Second,
	}, es, memory.NewProjectionStore())
	defer runRunner(t, r)()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for r.Status().Phase != projection.Stalled {
		if ctx.Err() != nil {
			t.Fatalf("runner never stalled; status %+v", r.Status())
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.mu.Lock()
	for gv := uint64(1); gv <= 4; gv++ {
		if !c.replayed[gv] {
			t.Fatalf("IsReplaying for event %d before Rebuild = false, want true", gv)
		}
	}
	c.mu.Unlock()

	poison.Store(false)
	if err := r.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if err := r.WaitUntil(ctx, last); err != nil {
		t.Fatalf("WaitUntil(%d): %v", last, err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.resets != 1 {
		t.Fatalf("Reset ran %d times, want 1", c.resets)
	}
	for gv := uint64(1); gv <= last; gv++ {
		if !c.replayed[gv] {
			t.Errorf("IsReplaying for event %d (in the log at the first start) after Rebuild = false, want true", gv)
		}
	}
}
