package slicetest

import (
	"context"
	"testing"
	"time"

	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/memory"
	"github.com/terraskye/eventsourcing/projection"
)

// ViewSlice tests one projection: given events, the read model it keeps
// looks like this. Build one with [View].
type ViewSlice struct {
	t      testing.TB
	log    *log
	runner *projection.Runner
}

// View starts def on memory stores and returns the slice. The projection
// runs in the background until the test ends; Given waits for it to catch
// up, so the read model is current when Then runs.
//
// Options: [IgnoreFields], [StreamID].
func View(t testing.TB, def projection.Definition, opts ...Option) *ViewSlice {
	t.Helper()
	l := newLog(t, newOptions(opts))
	v := &ViewSlice{t: t, log: l}
	v.runner = startProjection(t, def, l.store)
	return v
}

// startProjection runs def on reader until the test ends.
func startProjection(t testing.TB, def projection.Definition, reader eventsourcing.GlobalReader) *projection.Runner {
	t.Helper()
	runner := projection.NewRunner(def, reader, memory.NewProjectionStore())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = runner.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("slicetest: projection %q did not stop", def.Name)
		}
	})
	return runner
}

// waitLive blocks until runner has applied everything up to position.
func waitLive(t testing.TB, runner *projection.Runner, position uint64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := runner.WaitUntil(ctx, position); err != nil {
		t.Fatalf("slicetest: projection %q did not reach position %d: %v", runner.Name(), position, err)
	}
}

// Given records events and waits until the projection has applied them.
// Call it once, or several times, to build up history in steps.
func (v *ViewSlice) Given(events ...eventsourcing.Event) *ViewSlice {
	v.t.Helper()
	head := v.log.seed(events)
	waitLive(v.t, v.runner, head)
	return v
}

// Then runs the test's assertions on the read model. It exists for the
// reading order of a slice test; asserting directly after Given is the
// same thing.
func (v *ViewSlice) Then(check func(t testing.TB)) {
	v.t.Helper()
	check(v.t)
}

// Runner exposes the projection runner, for assertions on its status or
// to pass as a Dependency.
func (v *ViewSlice) Runner() *projection.Runner { return v.runner }

// Store exposes the memory event store.
func (v *ViewSlice) Store() eventsourcing.EventStore { return v.log.store }
