package projection_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/memory"
	"github.com/terraskye/eventsourcing/projection"
)

// testStore is a tiny in-process GlobalReader + EventStore pair for tests.
// It avoids pulling in the memory event store (and its otel dep) into the
// projection package test build.
type testStore struct {
	mu     sync.Mutex
	events []*eventsourcing.Envelope
}

func (s *testStore) Save(ctx context.Context, envs []eventsourcing.Envelope, state eventsourcing.StreamState) (eventsourcing.AppendResult, error) {
	_ = state
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range envs {
		envs[i].GlobalPosition = uint64(len(s.events) + 1)
		envs[i].OccurredAt = time.Now()
		copy := envs[i]
		s.events = append(s.events, &copy)
	}
	var res eventsourcing.AppendResult
	if n := len(envs); n > 0 {
		res.GlobalPosition = envs[n-1].GlobalPosition
	}
	return res, nil
}

func (s *testStore) ReadAll(ctx context.Context, after uint64, opts eventsourcing.ReadOptions) (eventsourcing.ReadResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	limit := opts.Limit
	if limit <= 0 {
		limit = 100
	}
	out := make([]*eventsourcing.Envelope, 0, limit)
	next := after
	for _, env := range s.events {
		if env.GlobalPosition <= after {
			continue
		}
		next = env.GlobalPosition
		if len(opts.EventTypes) > 0 {
			ok := false
			for _, t := range opts.EventTypes {
				if env.Event.EventType() == t {
					ok = true
					break
				}
			}
			if !ok {
				if len(out) >= limit {
					break
				}
				continue
			}
		}
		out = append(out, env)
		if len(out) >= limit {
			break
		}
	}
	atEnd := len(s.events) == 0 || next == uint64(len(s.events))
	return eventsourcing.ReadResult{Events: out, Next: next, AtEnd: atEnd}, nil
}

func (s *testStore) Head(context.Context) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return uint64(len(s.events)), nil
}

// Satisfy EventStore well enough to compile; tests don't use the stream
// methods.
func (s *testStore) Load(ctx context.Context, streamID string) ([]*eventsourcing.Envelope, error) {
	return nil, nil
}

type taskCreated struct{ ID string }

func (e *taskCreated) AggregateID() string { return e.ID }
func (e *taskCreated) EventType() string   { return "projection_test.TaskCreated" }

func init() {
	eventsourcing.RegisterEvent(&taskCreated{})
}

// counter is a simple in-memory read model.
type counter struct {
	mu       sync.Mutex
	applied  map[uint64]int
	replayed map[uint64]bool
	resets   int
	fail     func(gv uint64) error
}

func newCounter() *counter {
	return &counter{applied: map[uint64]int{}, replayed: map[uint64]bool{}}
}

func (c *counter) OnTaskCreated(ctx context.Context, e *taskCreated) error {
	gv := eventsourcing.GlobalPositionFromContext(ctx)
	if c.fail != nil {
		if err := c.fail(gv); err != nil {
			return err
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.applied[gv]++
	c.replayed[gv] = projection.IsReplaying(ctx)
	return nil
}

func (c *counter) Reset(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.applied = map[uint64]int{}
	c.replayed = map[uint64]bool{}
	c.resets++
	return nil
}

func (c *counter) Handle(ctx context.Context, event eventsourcing.Event) error {
	switch e := event.(type) {
	case *taskCreated:
		return c.OnTaskCreated(ctx, e)
	}
	return nil
}

var counterEvents = []string{"projection_test.TaskCreated"}

// save appends n events, returning the last GlobalPosition.
func save(t *testing.T, store *testStore, n int) uint64 {
	t.Helper()
	envs := make([]eventsourcing.Envelope, n)
	for i := 0; i < n; i++ {
		envs[i] = eventsourcing.Envelope{
			EventID:  uuid.New(),
			StreamID: "stream-1",
			Event:    &taskCreated{ID: "t"},
			Version:  uint64(i + 1),
		}
	}
	res, err := store.Save(t.Context(), envs, eventsourcing.Any{})
	if err != nil {
		t.Fatalf("Save(%d events) = %v", n, err)
	}
	return res.GlobalPosition
}

// runRunner starts r and returns a cancel func that waits for Run to exit.
func runRunner(t *testing.T, r *projection.Runner) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		_ = r.Run(ctx)
		close(done)
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("runner did not shut down")
		}
	}
}

func newRunner(c *counter, store *testStore, segments int) *projection.Runner {
	p := projection.Definition{
		Name:         "test",
		Handler:      eventsourcing.NewEventHandlerFunc(c.Handle),
		EventTypes:   counterEvents,
		Reset:        c.Reset,
		Version:      1,
		PollInterval: 10 * time.Millisecond,
		ClaimTTL:     time.Second,
	}
	if segments > 1 {
		p.Segments = segments
		p.SegmentKey = projection.ByStreamID
	}
	return projection.NewRunner(p, store, memory.NewProjectionStore())
}

func TestRunner_CatchesUpAndLive(t *testing.T) {
	es := &testStore{}
	last := save(t, es, 3)

	c := newCounter()
	r := newRunner(c, es, 1)
	defer runRunner(t, r)()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := r.WaitUntil(ctx, last); err != nil {
		t.Fatalf("WaitUntil: %v", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.applied) != 3 {
		t.Fatalf("applied %d events, want 3: %v", len(c.applied), c.applied)
	}
}

func TestRunner_WaitUntilReturnsOnLive(t *testing.T) {
	es := &testStore{}
	c := newCounter()
	r := newRunner(c, es, 1)
	defer runRunner(t, r)()

	// Append after Run has started so the runner must go through CatchingUp
	// -> Live naturally.
	time.Sleep(50 * time.Millisecond)
	last := save(t, es, 2)
	r.Nudge()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := r.WaitUntil(ctx, last); err != nil {
		t.Fatalf("WaitUntil: %v", err)
	}
	live, err := r.IsLive(ctx)
	if err != nil || !live {
		t.Fatalf("IsLive() = %v, %v; want true, nil", live, err)
	}
}

func TestRunner_Rebuild(t *testing.T) {
	es := &testStore{}
	last := save(t, es, 3)

	c := newCounter()
	r := newRunner(c, es, 1)
	defer runRunner(t, r)()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := r.WaitUntil(ctx, last); err != nil {
		t.Fatalf("initial WaitUntil: %v", err)
	}

	// Hold the replay until the status right after Rebuild has been checked.
	var rebuilding atomic.Bool
	gate := make(chan struct{})
	c.fail = func(uint64) error {
		if rebuilding.Load() {
			<-gate
		}
		return nil
	}
	rebuilding.Store(true)
	if err := r.Rebuild(ctx); err != nil {
		close(gate)
		t.Fatalf("Rebuild: %v", err)
	}
	st := r.Status()
	close(gate)
	if st.Position != 0 || st.Phase != projection.CatchingUp {
		t.Fatalf("Status() after Rebuild = %s at %d, want %s at 0", st.Phase, st.Position, projection.CatchingUp)
	}
	if err := r.WaitUntil(ctx, last); err != nil {
		t.Fatalf("post-rebuild WaitUntil: %v", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.resets != 1 {
		t.Fatalf("Reset ran %d times, want 1", c.resets)
	}
	for gv := uint64(1); gv <= 3; gv++ {
		if !c.replayed[gv] {
			t.Fatalf("IsReplaying for event %d after Rebuild = false, want true", gv)
		}
	}
}

func TestRunner_StallsOnError(t *testing.T) {
	es := &testStore{}
	save(t, es, 2)

	c := newCounter()
	failed := make(chan struct{}, 1)
	c.fail = func(gv uint64) error {
		if gv == 2 {
			select {
			case failed <- struct{}{}:
			default:
			}
			return errors.New("boom")
		}
		return nil
	}
	r := newRunner(c, es, 1)
	defer runRunner(t, r)()

	select {
	case <-failed:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never failed")
	}

	// Give the runner a moment to record Stalled status, then verify.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r.Status().Phase == projection.Stalled {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Status().Phase = %s, want %s", r.Status().Phase, projection.Stalled)
}

func TestRunner_Panics(t *testing.T) {
	tests := []struct {
		name string
		fn   func()
	}{
		{"empty name", func() {
			projection.NewRunner(projection.Definition{Handler: eventsourcing.NewEventHandlerFunc(newCounter().Handle), EventTypes: counterEvents}, &nopReader{}, memory.NewProjectionStore())
		}},
		{"nil handler", func() {
			projection.NewRunner(projection.Definition{Name: "x", EventTypes: counterEvents}, &nopReader{}, memory.NewProjectionStore())
		}},
		{"no events", func() {
			projection.NewRunner(projection.Definition{Name: "x", Handler: eventsourcing.NewEventHandlerFunc(newCounter().Handle)}, &nopReader{}, memory.NewProjectionStore())
		}},
		{"unregistered event", func() {
			projection.NewRunner(projection.Definition{Name: "x", Handler: eventsourcing.NewEventHandlerFunc(newCounter().Handle), EventTypes: []string{"Unregistered"}}, &nopReader{}, memory.NewProjectionStore())
		}},
		{"version without reset", func() {
			c := newCounter()
			projection.NewRunner(projection.Definition{Name: "x", Handler: eventsourcing.NewEventHandlerFunc(c.Handle), EventTypes: counterEvents, Version: 1}, &nopReader{}, memory.NewProjectionStore())
		}},
		{"segments without key", func() {
			c := newCounter()
			projection.NewRunner(projection.Definition{Name: "x", Handler: eventsourcing.NewEventHandlerFunc(c.Handle), EventTypes: counterEvents, Segments: 2}, &nopReader{}, memory.NewProjectionStore())
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatal("NewRunner did not panic")
				}
			}()
			tc.fn()
		})
	}
}

// TestRunner_Segmented covers that each segment processes only its own
// share and together they cover every event.
func TestRunner_Segmented(t *testing.T) {
	es := &testStore{}
	// Three streams, two events each.
	for _, sid := range []string{"a", "b", "c"} {
		envs := []eventsourcing.Envelope{
			{EventID: uuid.New(), StreamID: sid, Event: &taskCreated{ID: sid}, Version: 1},
			{EventID: uuid.New(), StreamID: sid, Event: &taskCreated{ID: sid}, Version: 2},
		}
		if _, err := es.Save(t.Context(), envs, eventsourcing.Any{}); err != nil {
			t.Fatal(err)
		}
	}

	c := newCounter()
	r := projection.NewRunner(projection.Definition{
		Name:         "test-seg",
		Handler:      eventsourcing.NewEventHandlerFunc(c.Handle),
		EventTypes:   counterEvents,
		Segments:     4,
		SegmentKey:   projection.ByStreamID,
		PollInterval: 10 * time.Millisecond,
		ClaimTTL:     time.Second,
	}, es, memory.NewProjectionStore())
	defer runRunner(t, r)()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := r.WaitUntil(ctx, 6); err != nil {
		t.Fatalf("WaitUntil: %v", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.applied) != 6 {
		t.Fatalf("segments applied %d events together, want 6: %v", len(c.applied), c.applied)
	}
}

type nopReader struct{}

func (*nopReader) ReadAll(context.Context, uint64, eventsourcing.ReadOptions) (eventsourcing.ReadResult, error) {
	return eventsourcing.ReadResult{AtEnd: true}, nil
}
func (*nopReader) Head(context.Context) (uint64, error) { return 0, nil }

// TestRunner_DefaultOwnerIsUniquePerRunner covers that two runners in one
// process, both on the default Owner, do not share a claim: a second runner
// started while the first holds the segment stays in Standby.
func TestRunner_DefaultOwnerIsUniquePerRunner(t *testing.T) {
	es := &testStore{}
	last := save(t, es, 3)
	store := memory.NewProjectionStore()

	newDefaultOwnerRunner := func(c *counter) *projection.Runner {
		return projection.NewRunner(projection.Definition{
			Name:         "test",
			Handler:      eventsourcing.NewEventHandlerFunc(c.Handle),
			EventTypes:   counterEvents,
			PollInterval: 10 * time.Millisecond,
			ClaimTTL:     time.Second,
		}, es, store)
	}
	c1, c2 := newCounter(), newCounter()
	r1, r2 := newDefaultOwnerRunner(c1), newDefaultOwnerRunner(c2)

	defer runRunner(t, r1)()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := r1.WaitUntil(ctx, last); err != nil {
		t.Fatalf("WaitUntil: %v", err)
	}

	defer runRunner(t, r2)()
	time.Sleep(100 * time.Millisecond) // several claim attempts by r2

	if st := r2.Status(); st.Phase != projection.Standby {
		t.Fatalf("second runner Status() = %s owned by %q, want %s while the first holds the segment", st.Phase, st.Owner, projection.Standby)
	}
	if st := r1.Status(); st.Owner == "" || st.Owner == r2.Status().Owner {
		t.Fatalf("first runner Owner = %q, want a non-empty owner distinct from the second's", st.Owner)
	}
}

// TestRunner_RebuildCallerContextDoesNotStopRun covers that a Rebuild whose
// own ctx ends mid-reset fails that Rebuild only; Run keeps going until its
// own ctx is done.
func TestRunner_RebuildCallerContextDoesNotStopRun(t *testing.T) {
	es := &testStore{}
	save(t, es, 3)
	c := newCounter()
	r := projection.NewRunner(projection.Definition{
		Name:       "test",
		Handler:    eventsourcing.NewEventHandlerFunc(c.Handle),
		EventTypes: counterEvents,
		Reset: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		},
		PollInterval: 10 * time.Millisecond,
		ClaimTTL:     time.Second,
	}, es, memory.NewProjectionStore())

	runCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(runCtx) }()
	time.Sleep(100 * time.Millisecond)

	ctx, cancelRebuild := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancelRebuild()
	_ = r.Rebuild(ctx)

	select {
	case err := <-done:
		t.Fatalf("Run returned %v after a Rebuild's ctx expired, while Run's own ctx is still live", err)
	case <-time.After(500 * time.Millisecond):
	}
}

// TestRunner_SkipWhenLiveAfterRestart covers that SkipWhenLive skips a
// failing new event even when the runner meets it right after a restart,
// before it has reported Live in this process.
func TestRunner_SkipWhenLiveAfterRestart(t *testing.T) {
	es := &testStore{}
	ps := memory.NewProjectionStore()
	c := newCounter()
	c.fail = func(uint64) error { return errors.New("poison") }
	newSkipRunner := func() *projection.Runner {
		return projection.NewRunner(projection.Definition{
			Name:         "test",
			Handler:      eventsourcing.NewEventHandlerFunc(c.Handle),
			EventTypes:   counterEvents,
			OnError:      projection.SkipWhenLive,
			PollInterval: 10 * time.Millisecond,
			ClaimTTL:     time.Second,
		}, es, ps)
	}

	// Run once on an empty log so the event appended next is new, not
	// replayed.
	stop := runRunner(t, newSkipRunner())
	time.Sleep(100 * time.Millisecond)
	stop()

	last := save(t, es, 1)
	r := newSkipRunner()
	defer runRunner(t, r)()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := r.WaitUntil(ctx, last); err != nil {
		t.Fatalf("WaitUntil: %v; status %+v", err, r.Status())
	}
	if n := r.Skipped(); n != 1 {
		t.Fatalf("Skipped() = %d, want 1", n)
	}
}

// TestRunner_ReplayUntilKeptAcrossRestart covers that events appended
// while a projection that first started on an empty log was down are new,
// not replayed: the replay boundary is the head at the first start, not at
// the restart.
func TestRunner_ReplayUntilKeptAcrossRestart(t *testing.T) {
	es := &testStore{}
	ps := memory.NewProjectionStore()
	c := newCounter()
	newPlain := func() *projection.Runner {
		return projection.NewRunner(projection.Definition{
			Name:         "test",
			Handler:      eventsourcing.NewEventHandlerFunc(c.Handle),
			EventTypes:   counterEvents,
			PollInterval: 10 * time.Millisecond,
			ClaimTTL:     time.Second,
		}, es, ps)
	}

	stop := runRunner(t, newPlain())
	time.Sleep(100 * time.Millisecond) // live on an empty log
	stop()

	last := save(t, es, 2)
	r := newPlain()
	defer runRunner(t, r)()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := r.WaitUntil(ctx, last); err != nil {
		t.Fatalf("WaitUntil(%d): %v", last, err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for gv := uint64(1); gv <= last; gv++ {
		if c.replayed[gv] {
			t.Errorf("IsReplaying for event %d appended after the first start = true, want false", gv)
		}
	}
}

// pausedStore drops a runner's status publishes while paused, as a process
// that stalls would not send them: its claim's heartbeat ages meanwhile.
type pausedStore struct {
	projection.Store
	paused atomic.Bool
}

func (s *pausedStore) SaveStatus(ctx context.Context, l projection.Lease, st projection.Status) error {
	if s.paused.Load() {
		return nil
	}
	return s.Store.SaveStatus(ctx, l, st)
}

// TestRunner_DropsSegmentWhenSaveStatusLosesClaim covers that a runner
// whose status publish is refused because another instance took the
// segment stops reporting the segment as its own.
func TestRunner_DropsSegmentWhenSaveStatusLosesClaim(t *testing.T) {
	es := &testStore{}
	ps := memory.NewProjectionStore()
	as := &pausedStore{Store: ps}
	const claimTTL = 100 * time.Millisecond
	r := projection.NewRunner(projection.Definition{
		Name:         "test",
		Handler:      eventsourcing.NewEventHandlerFunc(newCounter().Handle),
		EventTypes:   counterEvents,
		Owner:        "a",
		PollInterval: 10 * time.Millisecond,
		ClaimTTL:     claimTTL,
	}, es, as)
	defer runRunner(t, r)()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	for r.Status().Owner != "a" {
		if ctx.Err() != nil {
			t.Fatalf("runner never claimed the segment; status %+v", r.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Let a's claim expire by its own ttl, then take the segment over as b.
	as.paused.Store(true)
	time.Sleep(2 * claimTTL)
	if _, _, ok, err := ps.Claim(t.Context(), projection.Lease{Name: "test", Owner: "b"}, time.Hour); err != nil || !ok {
		t.Fatalf("Claim(b) = %v, %v; want true, nil", ok, err)
	}
	as.paused.Store(false)

	for r.Status().Phase != projection.Standby {
		if ctx.Err() != nil {
			t.Fatalf("Status() after b took the segment = %+v, want phase %s", r.Status(), projection.Standby)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestRunner_NotLiveAfterShutdown covers that a projection whose only
// runner has stopped is not reported Live to other instances.
func TestRunner_NotLiveAfterShutdown(t *testing.T) {
	es := &testStore{}
	ps := memory.NewProjectionStore()
	r := projection.NewRunner(projection.Definition{
		Name:         "test",
		Handler:      eventsourcing.NewEventHandlerFunc(newCounter().Handle),
		EventTypes:   counterEvents,
		PollInterval: 10 * time.Millisecond,
		ClaimTTL:     time.Second,
	}, es, ps)
	stop := runRunner(t, r)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	for {
		if live, _ := r.IsLive(ctx); live {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("runner never went live")
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop()

	// A runner of another instance answers from the store.
	other := projection.NewRunner(projection.Definition{
		Name:       "test",
		Handler:    eventsourcing.NewEventHandlerFunc(newCounter().Handle),
		EventTypes: counterEvents,
		Owner:      "other",
	}, es, ps)
	live, err := other.IsLive(t.Context())
	if err != nil {
		t.Fatalf("IsLive() on another instance: %v", err)
	}
	if live {
		t.Fatal("IsLive() on another instance = true after the only runner stopped, want false")
	}
}

// TestRunner_StatusHeadReportsLag covers that Status.Head is the head of the
// log, so Head minus Position is how far the projection lags.
func TestRunner_StatusHeadReportsLag(t *testing.T) {
	es := &testStore{}
	last := save(t, es, 10)
	c := newCounter()
	c.fail = func(gv uint64) error {
		if gv == 3 {
			return errors.New("poison")
		}
		return nil
	}
	r := projection.NewRunner(projection.Definition{
		Name:         "test",
		Handler:      eventsourcing.NewEventHandlerFunc(c.Handle),
		EventTypes:   counterEvents,
		BatchSize:    2,
		PollInterval: 10 * time.Millisecond,
		ClaimTTL:     time.Second,
	}, es, memory.NewProjectionStore())
	defer runRunner(t, r)()

	deadline := time.Now().Add(2 * time.Second)
	for r.Status().Phase != projection.Stalled {
		if time.Now().After(deadline) {
			t.Fatalf("runner never stalled; status %+v", r.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st := r.Status(); st.Head != last {
		t.Fatalf("Status.Head = %d at Position %d, want the head of the log, %d", st.Head, st.Position, last)
	}
}

type taskRenamed struct{ ID string }

func (e *taskRenamed) AggregateID() string { return e.ID }
func (e *taskRenamed) EventType() string   { return "projection_test.TaskRenamed" }

func init() {
	eventsourcing.RegisterEvent(&taskRenamed{})
}

// typeCounter counts the events it applies per type, and its resets.
type typeCounter struct {
	mu      sync.Mutex
	applied map[string]int
	resets  int
}

func (c *typeCounter) Handle(_ context.Context, e eventsourcing.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.applied[e.EventType()]++
	return nil
}

func (c *typeCounter) Reset(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.applied = map[string]int{}
	c.resets++
	return nil
}

// eventTypesChangeSetup appends two TaskCreated and one TaskRenamed, runs
// a projection over TaskCreated alone to the end of the log, and returns
// the log, the projection store and the last position.
func eventTypesChangeSetup(t *testing.T) (*testStore, *memory.ProjectionStore, uint64) {
	t.Helper()
	es := &testStore{}
	save(t, es, 2)
	res, err := es.Save(t.Context(), []eventsourcing.Envelope{{EventID: uuid.New(), StreamID: "stream-1", Event: &taskRenamed{ID: "t"}}}, eventsourcing.Any{})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	ps := memory.NewProjectionStore()

	first := &typeCounter{applied: map[string]int{}}
	r := projection.NewRunner(projection.Definition{
		Name:         "test",
		Handler:      first,
		EventTypes:   counterEvents,
		Reset:        first.Reset,
		PollInterval: 10 * time.Millisecond,
		ClaimTTL:     time.Second,
	}, es, ps)
	stop := runRunner(t, r)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := r.WaitUntil(ctx, res.GlobalPosition); err != nil {
		t.Fatalf("WaitUntil before the change: %v", err)
	}
	stop()
	return es, ps, res.GlobalPosition
}

// TestRunner_EventTypesChangeRebuilds covers that adding an event type
// without a Version bump rebuilds the projection when it has a Reset, so
// the new type's history is applied.
func TestRunner_EventTypesChangeRebuilds(t *testing.T) {
	es, ps, last := eventTypesChangeSetup(t)

	c := &typeCounter{applied: map[string]int{}}
	r := projection.NewRunner(projection.Definition{
		Name:         "test",
		Handler:      c,
		EventTypes:   []string{"projection_test.TaskCreated", "projection_test.TaskRenamed"},
		Reset:        c.Reset,
		PollInterval: 10 * time.Millisecond,
		ClaimTTL:     time.Second,
	}, es, ps)
	defer runRunner(t, r)()

	// WaitUntil alone could answer from the checkpoint the first runner
	// left in the store, before this runner claimed the segment; wait for
	// the replay itself.
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.mu.Lock()
		done := c.applied["projection_test.TaskRenamed"] == 1
		c.mu.Unlock()
		if done || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := r.WaitUntil(ctx, last); err != nil {
		t.Fatalf("WaitUntil after the change: %v", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.resets != 1 {
		t.Fatalf("Reset ran %d times after adding an event type, want 1", c.resets)
	}
	if c.applied["projection_test.TaskRenamed"] != 1 || c.applied["projection_test.TaskCreated"] != 2 {
		t.Fatalf("applied %v after the rebuild, want 2 TaskCreated and 1 TaskRenamed", c.applied)
	}
}

// TestRunner_EventTypesChangeWithoutResetStalls covers that a projection
// that cannot rebuild refuses to carry on over a changed EventTypes.
func TestRunner_EventTypesChangeWithoutResetStalls(t *testing.T) {
	es, ps, _ := eventTypesChangeSetup(t)

	c := &typeCounter{applied: map[string]int{}}
	r := projection.NewRunner(projection.Definition{
		Name:         "test",
		Handler:      c,
		EventTypes:   []string{"projection_test.TaskCreated", "projection_test.TaskRenamed"},
		PollInterval: 10 * time.Millisecond,
		ClaimTTL:     time.Second,
	}, es, ps)
	defer runRunner(t, r)()

	deadline := time.Now().Add(2 * time.Second)
	for r.Status().Phase != projection.Stalled {
		if time.Now().After(deadline) {
			t.Fatalf("Status() = %+v, want %s", r.Status(), projection.Stalled)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := r.Status().LastError; !strings.Contains(got, projection.ErrEventTypesChanged.Error()) {
		t.Fatalf("LastError = %q, want it to report ErrEventTypesChanged", got)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.applied) != 0 {
		t.Fatalf("applied %v over a changed EventTypes, want nothing", c.applied)
	}
}

// streamLog records the events it applies as "stream:type", and fails
// TaskCreated on stream "s1" while failing is set.
type streamLog struct {
	mu      sync.Mutex
	applied []string
	failing atomic.Bool
}

func (l *streamLog) Handle(ctx context.Context, e eventsourcing.Event) error {
	stream := eventsourcing.StreamIDFromContext(ctx)
	if _, ok := e.(*taskCreated); ok && stream == "s1" && l.failing.Load() {
		return errors.New("boom")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.applied = append(l.applied, stream+":"+e.EventType())
	return nil
}

func (l *streamLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.applied...)
}

// TestRunner_SkippedEventParksItsStream covers that skipping an event
// holds back the later events of its stream, records them as dead
// letters, and that ReplayStream applies them in order once the handler
// is fixed.
func TestRunner_SkippedEventParksItsStream(t *testing.T) {
	es := &testStore{}
	saveTo := func(stream string, e eventsourcing.Event) uint64 {
		t.Helper()
		res, err := es.Save(t.Context(), []eventsourcing.Envelope{{EventID: uuid.New(), StreamID: stream, Event: e}}, eventsourcing.Any{})
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		return res.GlobalPosition
	}
	saveTo("s1", &taskCreated{ID: "1"}) // fails, skipped
	saveTo("s2", &taskCreated{ID: "2"})
	last := saveTo("s1", &taskRenamed{ID: "1"}) // parked behind the skipped event

	ps := memory.NewProjectionStore()
	h := &streamLog{}
	h.failing.Store(true)
	r := projection.NewRunner(projection.Definition{
		Name:         "test",
		Handler:      h,
		EventTypes:   []string{"projection_test.TaskCreated", "projection_test.TaskRenamed"},
		OnError:      projection.SkipAlways,
		PollInterval: 10 * time.Millisecond,
		ClaimTTL:     time.Second,
	}, es, ps)
	defer runRunner(t, r)()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := r.WaitUntil(ctx, last); err != nil {
		t.Fatalf("WaitUntil: %v", err)
	}
	if got := h.snapshot(); !slices.Equal(got, []string{"s2:projection_test.TaskCreated"}) {
		t.Fatalf("applied %v, want only s2's event: s1's later event must wait for the skipped one", got)
	}
	letters, err := ps.LoadDeadLetters(t.Context(), "test", 0)
	if err != nil {
		t.Fatalf("LoadDeadLetters: %v", err)
	}
	if len(letters) != 2 || letters[0].StreamID != "s1" || letters[1].Error != projection.ErrStreamParked.Error() {
		t.Fatalf("dead letters = %+v, want s1's failed event and its parked successor", letters)
	}

	h.failing.Store(false)
	if err := r.ReplayStream(ctx, "s1"); err != nil {
		t.Fatalf("ReplayStream: %v", err)
	}
	want := []string{"s2:projection_test.TaskCreated", "s1:projection_test.TaskCreated", "s1:projection_test.TaskRenamed"}
	if got := h.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("applied %v after ReplayStream, want %v", got, want)
	}
	if letters, _ := ps.LoadDeadLetters(t.Context(), "test", 0); len(letters) != 0 {
		t.Fatalf("dead letters after ReplayStream = %+v, want none", letters)
	}

	// The stream is no longer parked: its next event applies as usual.
	next := saveTo("s1", &taskRenamed{ID: "1"})
	r.Nudge()
	if err := r.WaitUntil(ctx, next); err != nil {
		t.Fatalf("WaitUntil after ReplayStream: %v", err)
	}
	if got := h.snapshot(); len(got) != 4 || got[3] != "s1:projection_test.TaskRenamed" {
		t.Fatalf("applied %v, want s1's new event applied after the replay", got)
	}
}

// logRecorder is a slog.Handler that keeps every record.
type logRecorder struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *logRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (h *logRecorder) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *logRecorder) WithGroup(string) slog.Handler            { return h }
func (h *logRecorder) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *logRecorder) snapshot() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.records)
}

// attrs returns r's attributes by key.
func attrs(r slog.Record) map[string]slog.Value {
	m := map[string]slog.Value{}
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value
		return true
	})
	return m
}

// TestRunner_LogsLifecycle covers that the runner logs claiming and
// stalling with the segment's attributes, and stays silent while idle.
func TestRunner_LogsLifecycle(t *testing.T) {
	es := &testStore{}
	last := save(t, es, 2)
	logs := &logRecorder{}
	c := newCounter()
	r := projection.NewRunner(projection.Definition{
		Name:         "test",
		Handler:      eventsourcing.NewEventHandlerFunc(c.Handle),
		EventTypes:   counterEvents,
		Owner:        "owner-a",
		Logger:       slog.New(logs),
		PollInterval: 10 * time.Millisecond,
		ClaimTTL:     time.Second,
	}, es, memory.NewProjectionStore())
	stop := runRunner(t, r)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := r.WaitUntil(ctx, last); err != nil {
		t.Fatalf("WaitUntil: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // ten idle polls
	idle := logs.snapshot()
	if len(idle) != 1 || idle[0].Level != slog.LevelInfo {
		t.Fatalf("records after catching up and idling = %v, want one Info for the claim", idle)
	}
	a := attrs(idle[0])
	for _, key := range []string{"projection", "segment", "owner", "position", "head"} {
		if _, ok := a[key]; !ok {
			t.Errorf("claim record lacks attribute %q: %v", key, a)
		}
	}
	if a["projection"].String() != "test" || a["owner"].String() != "owner-a" {
		t.Errorf("claim record attributes = %v, want projection test and owner owner-a", a)
	}

	c.fail = func(uint64) error { return errors.New("boom") }
	save(t, es, 1)
	r.Nudge()
	deadline := time.Now().Add(2 * time.Second)
	for {
		recs := logs.snapshot()
		if n := len(recs); n > 1 && recs[n-1].Level == slog.LevelError {
			if a := attrs(recs[n-1]); !strings.Contains(a["error"].String(), "boom") {
				t.Errorf("stall record error = %q, want the handler's error", a["error"].String())
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no Error record after the handler failed: %v", recs)
		}
		time.Sleep(10 * time.Millisecond)
	}

	stop()
	recs := logs.snapshot()
	if lastRec := recs[len(recs)-1]; lastRec.Level != slog.LevelInfo || !strings.Contains(lastRec.Message, "released") {
		t.Fatalf("last record = %q (%s), want the Info for the release on shutdown", lastRec.Message, lastRec.Level)
	}
}

// TestRunner_RebuildResetFails covers a Reset that fails on the instance
// Rebuild is called on: Rebuild returns the error instead of waiting until
// ctx ends, and the request stays pending until a retry serves it.
func TestRunner_RebuildResetFails(t *testing.T) {
	es, ps := &testStore{}, memory.NewProjectionStore()
	last := save(t, es, 3)

	c := newCounter()
	var mu sync.Mutex
	failing := true
	p := projection.Definition{
		Name:         "test",
		Handler:      eventsourcing.NewEventHandlerFunc(c.Handle),
		EventTypes:   counterEvents,
		Owner:        "a",
		PollInterval: 10 * time.Millisecond,
		ClaimTTL:     time.Second,
		Version:      1,
		Reset: func(ctx context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			if failing {
				return errors.New("wipe failed")
			}
			return c.Reset(ctx)
		},
	}
	r := projection.NewRunner(p, es, ps)
	defer runRunner(t, r)()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := r.WaitUntil(ctx, last); err != nil {
		t.Fatalf("WaitUntil: %v", err)
	}

	err := r.Rebuild(ctx)
	if err == nil || ctx.Err() != nil {
		t.Fatalf("Rebuild with a failing Reset = %v, want the Reset's error before ctx ends", err)
	}
	if requested, done, _ := ps.Rebuilds(ctx, "test"); requested != 1 || done != 0 {
		t.Fatalf("Rebuilds = %d, %d; want 1, 0: the request stays pending", requested, done)
	}

	mu.Lock()
	failing = false
	mu.Unlock()
	waitRebuilt(t, ps, 1)
	if got := resets(c); got != 1 {
		t.Errorf("Reset ran %d times once it worked, want once", got)
	}
}

// TestRunner_NudgeWakesEverySegment covers that Nudge makes a live runner
// read now on every segment, so WaitUntil returns well within PollInterval.
func TestRunner_NudgeWakesEverySegment(t *testing.T) {
	for _, segments := range []int{1, 2, 4} {
		t.Run(fmt.Sprintf("segments=%d", segments), func(t *testing.T) {
			es := memory.NewEventStore(16)
			def := projection.Definition{
				Name:         "nudge",
				Handler:      eventsourcing.NewEventHandlerFunc(func(context.Context, eventsourcing.Event) error { return nil }),
				EventTypes:   []string{"projection_test.TaskCreated"},
				PollInterval: 10 * time.Second,
				ClaimTTL:     30 * time.Second,
			}
			if segments > 1 {
				def.Segments = segments
				def.SegmentKey = projection.ByStreamID
			}
			r := projection.NewRunner(def, es, memory.NewProjectionStore())
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() { _ = r.Run(ctx); close(done) }()
			defer func() { cancel(); <-done }()

			// Wait for every segment to reach Live at the empty head, then
			// give each time to block in its wait for a nudge or the poll.
			deadline := time.Now().Add(2 * time.Second)
			for r.Status().Phase != projection.Live {
				if time.Now().After(deadline) {
					t.Fatalf("never live: %+v", r.Status())
				}
				time.Sleep(5 * time.Millisecond)
			}
			time.Sleep(50 * time.Millisecond)

			res, err := es.Save(t.Context(), []eventsourcing.Envelope{{
				EventID: uuid.New(), StreamID: "s-1", Event: &taskCreated{ID: "t"}, Version: 1,
			}}, eventsourcing.Any{})
			if err != nil {
				t.Fatal(err)
			}
			r.Nudge()
			wctx, wcancel := context.WithTimeout(t.Context(), time.Second)
			defer wcancel()
			if err := r.WaitUntil(wctx, res.GlobalPosition); err != nil {
				t.Fatalf("after Nudge, WaitUntil(%d) = %v; want nil well within PollInterval (10s)", res.GlobalPosition, err)
			}
		})
	}
}

// newInstance returns a runner of the counter projection "test" on ps, as
// one instance of a program that may run several. Without reset, the
// projection has no Reset.
func newInstance(c *counter, es *testStore, ps projection.Store, owner string, reset bool) *projection.Runner {
	p := projection.Definition{
		Name:         "test",
		Handler:      eventsourcing.NewEventHandlerFunc(c.Handle),
		EventTypes:   counterEvents,
		Owner:        owner,
		PollInterval: 10 * time.Millisecond,
		ClaimTTL:     time.Second,
	}
	if reset {
		p.Reset, p.Version = c.Reset, 1
	}
	return projection.NewRunner(p, es, ps)
}

func resets(c *counter) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.resets
}

// waitRebuilt waits until the store marks rebuild request n done.
func waitRebuilt(t *testing.T, ps projection.Store, n uint64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, done, err := ps.Rebuilds(t.Context(), "test")
		if err != nil {
			t.Fatalf("Rebuilds: %v", err)
		}
		if done >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("rebuild request %d not served within 3s (done %d)", n, done)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRunner_RebuildBeforeDeclare(t *testing.T) {
	r := newRunner(newCounter(), &testStore{}, 1)
	if err := r.Rebuild(t.Context()); !errors.Is(err, projection.ErrUnknownProjection) {
		t.Fatalf("Rebuild before any runner declared the projection = %v, want ErrUnknownProjection", err)
	}
}

// TestRunner_RebuildFromAnotherInstance covers calling Rebuild on an
// instance that does not hold segment 0: the instance that does rebuilds.
func TestRunner_RebuildFromAnotherInstance(t *testing.T) {
	es, ps := &testStore{}, memory.NewProjectionStore()
	last := save(t, es, 3)

	holder, standby := newCounter(), newCounter()
	a := newInstance(holder, es, ps, "a", true)
	defer runRunner(t, a)()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := a.WaitUntil(ctx, last); err != nil {
		t.Fatalf("WaitUntil: %v", err)
	}
	b := newInstance(standby, es, ps, "b", true)
	defer runRunner(t, b)()

	if err := b.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild on the standby instance = %v, want nil", err)
	}
	if n, m := resets(holder), resets(standby); n != 1 || m != 0 {
		t.Errorf("Reset ran %d times on the holder and %d on the standby, want 1 and 0", n, m)
	}
	if err := a.WaitUntil(ctx, last); err != nil {
		t.Fatalf("WaitUntil after the rebuild: %v", err)
	}
}

// TestRunner_RebuildRequestOutlivesTheRunner covers a request made while no
// runner holds the projection, as from a web process or after a crash: the
// next runner to claim segment 0 serves it.
func TestRunner_RebuildRequestOutlivesTheRunner(t *testing.T) {
	es, ps := &testStore{}, memory.NewProjectionStore()
	last := save(t, es, 3)

	first := newCounter()
	stop := runRunner(t, newInstance(first, es, ps, "a", true))
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	waitPosition(ctx, t, ps, last)
	stop()

	n, err := ps.RequestRebuild(ctx, "test")
	if err != nil {
		t.Fatalf("RequestRebuild: %v", err)
	}
	statuses, _ := ps.LoadStatuses(ctx, "test")
	if len(statuses) == 0 || !statuses[0].RebuildRequested {
		t.Fatalf("statuses = %+v, want the request shown as pending", statuses)
	}

	next := newCounter()
	defer runRunner(t, newInstance(next, es, ps, "b", true))()
	waitRebuilt(t, ps, n)
	if got := resets(next); got != 1 {
		t.Errorf("Reset ran %d times on the next runner, want once", got)
	}
}

// TestRunner_RebuildRequestsShareARebuild covers that requests made before
// a rebuild runs are all served by it.
func TestRunner_RebuildRequestsShareARebuild(t *testing.T) {
	es, ps := &testStore{}, memory.NewProjectionStore()
	last := save(t, es, 3)

	stop := runRunner(t, newInstance(newCounter(), es, ps, "a", true))
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	waitPosition(ctx, t, ps, last)
	stop()

	var n uint64
	for range 3 {
		var err error
		if n, err = ps.RequestRebuild(ctx, "test"); err != nil {
			t.Fatalf("RequestRebuild: %v", err)
		}
	}
	c := newCounter()
	defer runRunner(t, newInstance(c, es, ps, "b", true))()
	waitRebuilt(t, ps, n)
	if got := resets(c); got != 1 {
		t.Errorf("Reset ran %d times for 3 requests, want once", got)
	}
}

// TestRunner_RebuildWithoutResetStaysPending covers a request for a
// projection with no Reset: the runner cannot serve it, so it stays
// pending and visible, and the projection keeps running.
func TestRunner_RebuildWithoutResetStaysPending(t *testing.T) {
	es, ps := &testStore{}, memory.NewProjectionStore()
	last := save(t, es, 3)

	defer runRunner(t, newInstance(newCounter(), es, ps, "a", false))()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	waitPosition(ctx, t, ps, last)

	if _, err := ps.RequestRebuild(ctx, "test"); err != nil {
		t.Fatalf("RequestRebuild: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // ten of the runner's checks

	if requested, done, _ := ps.Rebuilds(ctx, "test"); requested != 1 || done != 0 {
		t.Errorf("Rebuilds = %d, %d; want 1, 0: nothing can serve it", requested, done)
	}
	more := save(t, es, 2)
	waitPosition(ctx, t, ps, more)
}

// waitPosition waits until segment 0 of "test" has processed position.
func waitPosition(ctx context.Context, t *testing.T, ps projection.Store, position uint64) {
	t.Helper()
	if err := projection.NewProbe(ps, "test", projection.WithPollInterval(5*time.Millisecond)).WaitUntil(ctx, position); err != nil {
		t.Fatalf("waiting for position %d: %v", position, err)
	}
}

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
			t.Errorf("IsReplaying for event %d before Rebuild = false, want true", gv)
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
