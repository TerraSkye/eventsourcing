package projection_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/projection"
	"github.com/terraskye/eventsourcing/projection/memory"
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
		envs[i].GlobalVersion = uint64(len(s.events) + 1)
		envs[i].OccurredAt = time.Now()
		copy := envs[i]
		s.events = append(s.events, &copy)
	}
	var res eventsourcing.AppendResult
	if n := len(envs); n > 0 {
		res.GlobalVersion = envs[n-1].GlobalVersion
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
		if env.GlobalVersion <= after {
			continue
		}
		next = env.GlobalVersion
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
	gv := eventsourcing.GlobalVersionFromContext(ctx)
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

// save appends n events, returning the last GlobalVersion.
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
	return res.GlobalVersion
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
	p := projection.Projection{
		Name:         "test",
		Handler:      eventsourcing.NewEventHandlerFunc(c.Handle),
		Events:       counterEvents,
		Reset:        c.Reset,
		Version:      1,
		PollInterval: 10 * time.Millisecond,
		ClaimTTL:     time.Second,
	}
	if segments > 1 {
		p.Segments = segments
		p.SegmentKey = projection.ByStreamID
	}
	return projection.NewRunner(p, store, memory.NewStore())
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
	live, err := r.Live(ctx)
	if err != nil || !live {
		t.Fatalf("Live() = %v, %v; want true, nil", live, err)
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

	if err := r.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild: %v", err)
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
			projection.NewRunner(projection.Projection{Handler: eventsourcing.NewEventHandlerFunc(newCounter().Handle), Events: counterEvents}, &nopReader{}, memory.NewStore())
		}},
		{"nil handler", func() {
			projection.NewRunner(projection.Projection{Name: "x", Events: counterEvents}, &nopReader{}, memory.NewStore())
		}},
		{"no events", func() {
			projection.NewRunner(projection.Projection{Name: "x", Handler: eventsourcing.NewEventHandlerFunc(newCounter().Handle)}, &nopReader{}, memory.NewStore())
		}},
		{"unregistered event", func() {
			projection.NewRunner(projection.Projection{Name: "x", Handler: eventsourcing.NewEventHandlerFunc(newCounter().Handle), Events: []string{"Unregistered"}}, &nopReader{}, memory.NewStore())
		}},
		{"version without reset", func() {
			c := newCounter()
			projection.NewRunner(projection.Projection{Name: "x", Handler: eventsourcing.NewEventHandlerFunc(c.Handle), Events: counterEvents, Version: 1}, &nopReader{}, memory.NewStore())
		}},
		{"segments without key", func() {
			c := newCounter()
			projection.NewRunner(projection.Projection{Name: "x", Handler: eventsourcing.NewEventHandlerFunc(c.Handle), Events: counterEvents, Segments: 2}, &nopReader{}, memory.NewStore())
		}},
		{"segments with version", func() {
			c := newCounter()
			projection.NewRunner(projection.Projection{
				Name: "x", Handler: eventsourcing.NewEventHandlerFunc(c.Handle), Events: counterEvents,
				Segments: 2, SegmentKey: projection.ByStreamID, Version: 1, Reset: c.Reset,
			}, &nopReader{}, memory.NewStore())
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
	r := projection.NewRunner(projection.Projection{
		Name:         "test-seg",
		Handler:      eventsourcing.NewEventHandlerFunc(c.Handle),
		Events:       counterEvents,
		Segments:     4,
		SegmentKey:   projection.ByStreamID,
		PollInterval: 10 * time.Millisecond,
		ClaimTTL:     time.Second,
	}, es, memory.NewStore())
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

// TestRunner_RebuildCallerContextDoesNotStopRun covers that a Rebuild whose
// own ctx ends mid-reset fails that Rebuild only; Run keeps going until its
// own ctx is done.
func TestRunner_RebuildCallerContextDoesNotStopRun(t *testing.T) {
	es := &testStore{}
	save(t, es, 3)
	c := newCounter()
	r := projection.NewRunner(projection.Projection{
		Name:    "test",
		Handler: eventsourcing.NewEventHandlerFunc(c.Handle),
		Events:  counterEvents,
		Reset: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		},
		PollInterval: 10 * time.Millisecond,
		ClaimTTL:     time.Second,
	}, es, memory.NewStore())

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
	ps := memory.NewStore()
	c := newCounter()
	c.fail = func(uint64) error { return errors.New("poison") }
	newSkipRunner := func() *projection.Runner {
		return projection.NewRunner(projection.Projection{
			Name:         "test",
			Handler:      eventsourcing.NewEventHandlerFunc(c.Handle),
			Events:       counterEvents,
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
	ps := memory.NewStore()
	c := newCounter()
	newPlain := func() *projection.Runner {
		return projection.NewRunner(projection.Projection{
			Name:         "test",
			Handler:      eventsourcing.NewEventHandlerFunc(c.Handle),
			Events:       counterEvents,
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
	ps := memory.NewStore()
	r := projection.NewRunner(projection.Projection{
		Name:         "test",
		Handler:      eventsourcing.NewEventHandlerFunc(newCounter().Handle),
		Events:       counterEvents,
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

	// Take the segment over: release it as a, then claim it as b.
	if err := ps.Release(t.Context(), projection.Lease{Name: "test", Owner: "a"}); err != nil {
		t.Fatalf("Release(a): %v", err)
	}
	if _, ok, err := ps.Claim(t.Context(), projection.Lease{Name: "test", Owner: "b"}, time.Hour); err != nil || !ok {
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
	ps := memory.NewStore()
	r := projection.NewRunner(projection.Projection{
		Name:         "test",
		Handler:      eventsourcing.NewEventHandlerFunc(newCounter().Handle),
		Events:       counterEvents,
		PollInterval: 10 * time.Millisecond,
		ClaimTTL:     time.Second,
	}, es, ps)
	stop := runRunner(t, r)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	for {
		if live, _ := r.Live(ctx); live {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("runner never went live")
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop()

	// A runner of another instance answers from the store.
	other := projection.NewRunner(projection.Projection{
		Name:    "test",
		Handler: eventsourcing.NewEventHandlerFunc(newCounter().Handle),
		Events:  counterEvents,
		Owner:   "other",
	}, es, ps)
	live, err := other.Live(t.Context())
	if err != nil {
		t.Fatalf("Live() on another instance: %v", err)
	}
	if live {
		t.Fatal("Live() on another instance = true after the only runner stopped, want false")
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
	r := projection.NewRunner(projection.Projection{
		Name:         "test",
		Handler:      eventsourcing.NewEventHandlerFunc(c.Handle),
		Events:       counterEvents,
		BatchSize:    2,
		PollInterval: 10 * time.Millisecond,
		ClaimTTL:     time.Second,
	}, es, memory.NewStore())
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
