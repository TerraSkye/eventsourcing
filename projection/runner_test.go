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
	res, err := store.Save(context.Background(), envs, eventsourcing.Any{})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	return res.GlobalVersion
}

// runRunner starts r and returns a cancel func that waits for Run to exit.
func runRunner(t *testing.T, r *projection.Runner) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
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

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.WaitUntil(ctx, last); err != nil {
		t.Fatalf("WaitUntil: %v", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.applied) != 3 {
		t.Fatalf("want 3 applied, got %d: %v", len(c.applied), c.applied)
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

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.WaitUntil(ctx, last); err != nil {
		t.Fatalf("WaitUntil: %v", err)
	}
	live, err := r.Live(ctx)
	if err != nil || !live {
		t.Fatalf("want live=true nil, got %v %v", live, err)
	}
}

func TestRunner_Rebuild(t *testing.T) {
	es := &testStore{}
	last := save(t, es, 3)

	c := newCounter()
	r := newRunner(c, es, 1)
	defer runRunner(t, r)()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
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
		t.Fatalf("want 1 reset, got %d", c.resets)
	}
	for gv := uint64(1); gv <= 3; gv++ {
		if !c.replayed[gv] {
			t.Fatalf("event %d should be marked replayed after rebuild", gv)
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
	t.Fatalf("want Stalled, got %s", r.Status().Phase)
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
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatal("expected panic")
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
		if _, err := es.Save(context.Background(), envs, eventsourcing.Any{}); err != nil {
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

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.WaitUntil(ctx, 6); err != nil {
		t.Fatalf("WaitUntil: %v", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.applied) != 6 {
		t.Fatalf("want all 6 events applied across segments, got %d: %v", len(c.applied), c.applied)
	}
}

type nopReader struct{}

func (*nopReader) ReadAll(context.Context, uint64, eventsourcing.ReadOptions) (eventsourcing.ReadResult, error) {
	return eventsourcing.ReadResult{AtEnd: true}, nil
}
func (*nopReader) Head(context.Context) (uint64, error) { return 0, nil }
