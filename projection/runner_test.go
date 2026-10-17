package projection_test

import (
	"context"
	"errors"
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
		{"segments with version", func() {
			c := newCounter()
			projection.NewRunner(projection.Definition{
				Name: "x", Handler: eventsourcing.NewEventHandlerFunc(c.Handle), EventTypes: counterEvents,
				Segments: 2, SegmentKey: projection.ByStreamID, Version: 1, Reset: c.Reset,
			}, &nopReader{}, memory.NewProjectionStore())
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

// TestRunner_RebuildNotActive covers that Rebuild on a runner that is not
// running returns ErrNotActive instead of blocking until ctx is done.
func TestRunner_RebuildNotActive(t *testing.T) {
	r := newRunner(newCounter(), &testStore{}, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if err := r.Rebuild(ctx); !errors.Is(err, projection.ErrNotActive) {
		t.Fatalf("Rebuild on a runner that is not running = %v, want ErrNotActive", err)
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

// TestRunner_DropsSegmentWhenSaveStatusLosesClaim covers that a runner
// whose status publish is refused because another instance took the
// segment stops reporting the segment as its own.
func TestRunner_DropsSegmentWhenSaveStatusLosesClaim(t *testing.T) {
	es := &testStore{}
	ps := memory.NewProjectionStore()
	r := projection.NewRunner(projection.Definition{
		Name:         "test",
		Handler:      eventsourcing.NewEventHandlerFunc(newCounter().Handle),
		EventTypes:   counterEvents,
		Owner:        "a",
		PollInterval: 10 * time.Millisecond,
		ClaimTTL:     time.Hour,
	}, es, ps)
	defer runRunner(t, r)()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	for r.Status().Owner != "a" {
		if ctx.Err() != nil {
			t.Fatalf("runner never claimed the segment; status %+v", r.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Take the segment over as b: with a ttl of 1ns, a's heartbeat counts
	// as expired.
	if _, _, ok, err := ps.Claim(t.Context(), projection.Lease{Name: "test", Owner: "b"}, time.Nanosecond); err != nil || !ok {
		t.Fatalf("Claim(b) = %v, %v; want true, nil", ok, err)
	}

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
