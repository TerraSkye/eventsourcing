package projection_test

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/memory"
	"github.com/terraskye/eventsourcing/projection"
)

// rowsModel is a read model with one row per stream, shared by every
// runner in a test the way a database table is shared by instances.
type rowsModel struct {
	mu     sync.Mutex
	rows   map[string]uint64 // stream ID -> last GlobalPosition applied
	resets int
}

func newRowsModel() *rowsModel { return &rowsModel{rows: map[string]uint64{}} }

func (m *rowsModel) Handle(ctx context.Context, _ eventsourcing.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[eventsourcing.StreamIDFromContext(ctx)] = eventsourcing.GlobalPositionFromContext(ctx)
	return nil
}

func (m *rowsModel) Reset(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows = map[string]uint64{}
	m.resets++
	return nil
}

func (m *rowsModel) snapshot() (rows int, resets int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rows), m.resets
}

// saveStreams appends one event to each of n streams and returns the last
// GlobalPosition.
func saveStreams(t *testing.T, store *testStore, n int) uint64 {
	t.Helper()
	var last uint64
	for i := range n {
		stream := fmt.Sprintf("stream-%d", i)
		res, err := store.Save(t.Context(), []eventsourcing.Envelope{{
			EventID:  uuid.New(),
			StreamID: stream,
			Event:    &taskCreated{ID: stream},
			Version:  1,
		}}, eventsourcing.Any{})
		if err != nil {
			t.Fatalf("Save(%s) = %v", stream, err)
		}
		last = res.GlobalPosition
	}
	return last
}

func newRowsRunner(m *rowsModel, es *testStore, ps projection.Store, owner string, version, segments int) *projection.Runner {
	p := projection.Definition{
		Name:         "rows",
		Owner:        owner,
		Handler:      eventsourcing.NewEventHandlerFunc(m.Handle),
		EventTypes:   counterEvents,
		Reset:        m.Reset,
		Version:      version,
		PollInterval: 10 * time.Millisecond,
		ClaimTTL:     time.Second,
	}
	if segments > 1 {
		p.Segments = segments
		p.SegmentKey = projection.ByStreamID
	}
	return projection.NewRunner(p, es, ps)
}

// TestRunner_SegmentedVersionBumpKeepsEveryRow covers rebuilding a
// segmented projection in place. Definition.Reset wipes the whole read
// model, so a per-segment reset would let one segment's reset delete rows
// another segment had already rebuilt. Store.Reset covers every segment at
// once instead, and other instances are fenced off and resume from the
// reset checkpoint.
func TestRunner_SegmentedVersionBumpKeepsEveryRow(t *testing.T) {
	const streams, segments = 40, 4
	es := &testStore{}
	ps := memory.NewProjectionStore()
	m := newRowsModel()
	last := saveStreams(t, es, streams)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	v1 := newRowsRunner(m, es, ps, "v1", 1, segments)
	stop := runRunner(t, v1)
	if err := v1.WaitUntil(ctx, last); err != nil {
		t.Fatalf("WaitUntil on version 1: %v", err)
	}
	stop()

	// Two instances of the next deployment share the segments.
	a := newRowsRunner(m, es, ps, "v2-a", 2, segments)
	b := newRowsRunner(m, es, ps, "v2-b", 2, segments)
	defer runRunner(t, a)()
	defer runRunner(t, b)()

	probe := projection.NewProbe(projection.ProbeDefinition{Name: "rows", PollInterval: 5 * time.Millisecond}, ps)
	for {
		statuses, err := ps.LoadStatuses(ctx, "rows")
		if err != nil {
			t.Fatalf("LoadStatuses: %v", err)
		}
		rebuilt := len(statuses) == segments
		for _, st := range statuses {
			rebuilt = rebuilt && st.Version == 2
		}
		if rebuilt {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("projection never reached version 2; statuses %+v", statuses)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := probe.WaitUntil(ctx, last); err != nil {
		t.Fatalf("WaitUntil after the version bump: %v", err)
	}

	rows, resets := m.snapshot()
	if rows != streams {
		t.Errorf("read model holds %d rows after the rebuild, want all %d", rows, streams)
	}
	if resets != 1 {
		t.Errorf("read model was wiped %d times, want once for the whole projection", resets)
	}
}

// TestRunner_OlderVersionStandsBy covers a runner of an older deployment,
// still running during a rollout, meeting a checkpoint a newer one has
// rebuilt: it stands by instead of rebuilding back to its own version.
func TestRunner_OlderVersionStandsBy(t *testing.T) {
	es := &testStore{}
	ps := memory.NewProjectionStore()
	last := saveStreams(t, es, 3)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	newer := newRowsRunner(newRowsModel(), es, ps, "v2", 2, 1)
	stop := runRunner(t, newer)
	if err := newer.WaitUntil(ctx, last); err != nil {
		t.Fatalf("WaitUntil on version 2: %v", err)
	}
	stop()

	m := newRowsModel()
	older := newRowsRunner(m, es, ps, "v1", 1, 1)
	defer runRunner(t, older)()

	// Give the older runner several poll intervals to claim the free
	// segment and act on it.
	time.Sleep(100 * time.Millisecond)
	if _, resets := m.snapshot(); resets != 0 {
		t.Fatalf("older runner wiped the read model %d times, want it to leave the newer version alone", resets)
	}
	if got := older.Status().Phase; got != projection.Standby {
		t.Errorf("older runner Phase = %s, want %s", got, projection.Standby)
	}
	statuses, err := ps.LoadStatuses(ctx, "rows")
	if err != nil {
		t.Fatalf("LoadStatuses: %v", err)
	}
	if statuses[0].Version != 2 || statuses[0].Position < last {
		t.Errorf("stored status after the older runner ran: %+v, want version 2 at %d or past", statuses[0], last)
	}
}

// parkS1 appends to stream s1 an event the handler applies, one it fails
// on while failing, and one parked behind that, and returns the last
// GlobalPosition.
func parkS1(t *testing.T, es *testStore) uint64 {
	t.Helper()
	var last uint64
	for _, e := range []eventsourcing.Event{&taskRenamed{ID: "1"}, &taskCreated{ID: "1"}, &taskRenamed{ID: "1"}} {
		res, err := es.Save(t.Context(), []eventsourcing.Envelope{{EventID: uuid.New(), StreamID: "s1", Event: e}}, eventsourcing.Any{})
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		last = res.GlobalPosition
	}
	return last
}

func newLogRunner(h *streamLog, es *testStore, ps projection.Store, version int) *projection.Runner {
	return projection.NewRunner(projection.Definition{
		Name:       "log",
		Handler:    h,
		EventTypes: []string{"projection_test.TaskCreated", "projection_test.TaskRenamed"},
		OnError:    projection.SkipAlways,
		Version:    version,
		Reset: func(context.Context) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.applied = nil
			return nil
		},
		PollInterval: 10 * time.Millisecond,
		ClaimTTL:     time.Second,
	}, es, ps)
}

// TestRunner_RebuildReplaysParkedStream covers that a rebuild runs a parked
// stream's events through the handler again. Left in place, its dead
// letters would park the stream for the whole replay, dropping even the
// events applied before the failure, and record every event again.
func TestRunner_RebuildReplaysParkedStream(t *testing.T) {
	es := &testStore{}
	ps := memory.NewProjectionStore()
	last := parkS1(t, es)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	h := &streamLog{}
	h.failing.Store(true)
	v1 := newLogRunner(h, es, ps, 1)
	stop := runRunner(t, v1)
	if err := v1.WaitUntil(ctx, last); err != nil {
		t.Fatalf("WaitUntil on version 1: %v", err)
	}
	stop()
	if letters, _ := ps.LoadDeadLetters(ctx, "log", 0); len(letters) != 2 {
		t.Fatalf("dead letters on version 1 = %+v, want the failed event and its parked successor", letters)
	}

	// The handler is fixed and the version bumped.
	h.failing.Store(false)
	v2 := newLogRunner(h, es, ps, 2)
	defer runRunner(t, v2)()
	for {
		statuses, err := ps.LoadStatuses(ctx, "log")
		if err != nil {
			t.Fatalf("LoadStatuses: %v", err)
		}
		if statuses[0].Version == 2 && statuses[0].Position >= last {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("version 2 never caught up; status %+v", statuses[0])
		}
		time.Sleep(5 * time.Millisecond)
	}

	want := []string{"s1:projection_test.TaskRenamed", "s1:projection_test.TaskCreated", "s1:projection_test.TaskRenamed"}
	if got := h.snapshot(); !slices.Equal(got, want) {
		t.Errorf("applied after the rebuild = %v, want every event of s1 %v", got, want)
	}
	if letters, _ := ps.LoadDeadLetters(ctx, "log", 0); len(letters) != 0 {
		t.Errorf("dead letters after the rebuild = %+v, want none", letters)
	}
}

// TestRunner_ReplayStreamAppliesEachPositionOnce covers a position that
// carries two dead letters, which the memory store leaves when a Commit
// fails after recording one: ReplayStream applies the event once.
func TestRunner_ReplayStreamAppliesEachPositionOnce(t *testing.T) {
	es := &testStore{}
	ps := memory.NewProjectionStore()
	last := parkS1(t, es)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	h := &streamLog{}
	h.failing.Store(true)
	r := newLogRunner(h, es, ps, 0)
	stop := runRunner(t, r)
	if err := r.WaitUntil(ctx, last); err != nil {
		t.Fatalf("WaitUntil: %v", err)
	}
	stop()

	// Record the failed event a second time, keeping the checkpoint.
	letters, err := ps.LoadDeadLetters(ctx, "log", 0)
	if err != nil || len(letters) == 0 {
		t.Fatalf("LoadDeadLetters = %+v, %v", letters, err)
	}
	l, cp, ok, err := ps.Claim(ctx, projection.Lease{Name: "log", Owner: "manual"}, time.Second)
	if err != nil || !ok {
		t.Fatalf("Claim = %v, %v", ok, err)
	}
	statuses, _ := ps.LoadStatuses(ctx, "log")
	if err := ps.Commit(ctx, l, cp, statuses[0], func(ctx context.Context) error {
		return ps.SaveDeadLetter(ctx, l, letters[0])
	}); err != nil {
		t.Fatalf("Commit with a duplicate dead letter: %v", err)
	}
	if err := ps.Release(ctx, l); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// The memory store applies a failed batch's earlier events again when
	// it retries them one by one, so look only at what the replay applies.
	h.failing.Store(false)
	h.mu.Lock()
	h.applied = nil
	h.mu.Unlock()
	r = newLogRunner(h, es, ps, 0)
	defer runRunner(t, r)()
	if err := r.ReplayStream(ctx, "s1"); err != nil {
		t.Fatalf("ReplayStream: %v", err)
	}

	want := []string{"s1:projection_test.TaskCreated", "s1:projection_test.TaskRenamed"}
	if got := h.snapshot(); !slices.Equal(got, want) {
		t.Errorf("ReplayStream applied %v, want each dead letter's event once %v", got, want)
	}
}
