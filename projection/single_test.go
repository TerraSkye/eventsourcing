package projection

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/eventstore/memory"
)

type taskCreated struct{ ID string }

func (e *taskCreated) AggregateID() string { return e.ID }
func (e *taskCreated) EventType() string   { return "projection_test.TaskCreated" }

type taskNoted struct{ ID string }

func (e *taskNoted) AggregateID() string { return e.ID }
func (e *taskNoted) EventType() string   { return "projection_test.TaskNoted" }

func init() {
	eventsourcing.RegisterEvent(&taskCreated{})
	eventsourcing.RegisterEvent(&taskNoted{})
}

// counter is an in-memory read model: how often each position was applied,
// and whether it was applied as replayed.
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
	replaying := IsReplaying(ctx)
	write := func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.applied[gv]++
		c.replayed[gv] = replaying
	}
	if tx, ok := ctx.Value(txKey{}).(*testTx); ok {
		tx.writes = append(tx.writes, write)
		return nil
	}
	write()
	return nil
}

// txCheckpoints is an in-memory CheckpointStore that rolls back: handlers
// queue their writes on a testTx, which is applied only if the batch
// succeeds. It lets the tests check exactly-once, which the default memory
// checkpoints cannot give.
type txCheckpoints struct {
	memoryCheckpoints
}

type txKey struct{}

type testTx struct{ writes []func() }

func (s *txCheckpoints) Commit(ctx context.Context, name string, cp Checkpoint, fn func(ctx context.Context) error) error {
	tx := &testTx{}
	return s.memoryCheckpoints.Commit(ctx, name, cp, func(ctx context.Context) error {
		if err := fn(context.WithValue(ctx, txKey{}, tx)); err != nil {
			return err
		}
		for _, w := range tx.writes {
			w()
		}
		return nil
	})
}

func (c *counter) Reset(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.applied = map[uint64]int{}
	c.replayed = map[uint64]bool{}
	c.resets++
	return nil
}

func (c *counter) handlers() *eventsourcing.EventGroupProcessor {
	return eventsourcing.NewEventGroupProcessor(eventsourcing.OnEvent(c.OnTaskCreated))
}

func (c *counter) snapshot() (map[uint64]int, map[uint64]bool, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, r := map[uint64]int{}, map[uint64]bool{}
	for k, v := range c.applied {
		a[k] = v
	}
	for k, v := range c.replayed {
		r[k] = v
	}
	return a, r, c.resets
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func appendTasks(t *testing.T, store *memory.MemoryStore, n int) uint64 {
	t.Helper()
	for i := range n {
		id := fmt.Sprintf("task-%d", i%17)
		var ev eventsourcing.Event = &taskCreated{ID: id}
		if i%5 == 4 {
			ev = &taskNoted{ID: id} // not handled: filtered out by the runner
		}
		if _, err := store.Save(context.Background(), []eventsourcing.Envelope{{
			EventID: uuid.New(), StreamID: id, Event: ev,
		}}, eventsourcing.Any{}); err != nil {
			t.Fatal(err)
		}
	}
	head, _ := store.Head(context.Background())
	return head
}

// handledPositions lists the positions of the events the counter handles.
func handledPositions(t *testing.T, store *memory.MemoryStore) []uint64 {
	t.Helper()
	res, err := store.ReadAll(t.Context(), 0, eventsourcing.ReadOptions{Limit: 1 << 20, EventTypes: (&counter{}).handlers().StreamFilter()})
	if err != nil {
		t.Fatal(err)
	}
	var ps []uint64
	for _, e := range res.Events {
		ps = append(ps, e.GlobalVersion)
	}
	return ps
}

func run(t *testing.T, r *Runner) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Errorf("Run returned %v, want context.Canceled", err)
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func waitFor(t *testing.T, r *Runner, pos uint64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := r.WaitUntil(ctx, pos); err != nil {
		t.Fatalf("WaitUntil(%d): %v (status %+v)", pos, err, r.Status())
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func checkExactlyOnce(t *testing.T, c *counter, store *memory.MemoryStore) {
	t.Helper()
	applied, _, _ := c.snapshot()
	want := handledPositions(t, store)
	if len(applied) != len(want) {
		t.Errorf("applied %d events, want %d", len(applied), len(want))
	}
	for _, p := range want {
		if applied[p] != 1 {
			t.Errorf("event %d applied %d times, want 1", p, applied[p])
		}
	}
}

func TestRunner_CatchesUpGoesLiveAndFollows(t *testing.T) {
	store := memory.NewMemoryStore(0)
	head := appendTasks(t, store, 1234)
	c := newCounter()
	r := NewRunner("tasks", c.handlers(), store, WithBatchSize(100), WithPollInterval(time.Hour), WithLogger(quiet))

	var watched atomic.Int32
	r.Watch(func() { watched.Add(1) })
	run(t, r)

	waitFor(t, r, head)
	eventually(t, "live", func() bool { ok, _ := r.Live(t.Context()); return ok })
	checkExactlyOnce(t, c, store)
	if watched.Load() == 0 {
		t.Error("Watch callback never called")
	}

	// The memory store notifies, so new events arrive despite the hour-long poll.
	head = appendTasks(t, store, 50)
	waitFor(t, r, head)
	checkExactlyOnce(t, c, store)

	_, replayed, _ := c.snapshot()
	for p, rep := range replayed {
		if want := p <= 1234; rep != want {
			t.Errorf("IsReplaying for event %d = %v, want %v", p, rep, want)
		}
	}
	if s := r.Status(); s.Phase != Live || s.Position != head || s.Head < head {
		t.Errorf("status = %+v, want live at %d", s, head)
	}
}

func TestRunner_RebuildReplaysEverythingOnce(t *testing.T) {
	store := memory.NewMemoryStore(0)
	head := appendTasks(t, store, 300)
	c := newCounter()
	r := NewRunner("tasks", c.handlers(), store, WithReset(c.Reset), WithBatchSize(64), WithLogger(quiet))

	if err := r.Rebuild(t.Context()); !errors.Is(err, ErrNotActive) {
		t.Fatalf("Rebuild before Run = %v, want ErrNotActive", err)
	}
	run(t, r)
	waitFor(t, r, head)
	eventually(t, "live", func() bool { return r.Status().Phase == Live })

	if err := r.Rebuild(t.Context()); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	waitFor(t, r, head)
	eventually(t, "live after rebuild", func() bool { return r.Status().Phase == Live })
	checkExactlyOnce(t, c, store)
	if _, replayed, resets := c.snapshot(); resets != 1 || !replayed[handledPositions(t, store)[0]] {
		t.Errorf("resets = %d, replayed after rebuild = %v; want 1 reset and replayed events", resets, replayed[1])
	}

	noReset := NewRunner("other", newCounter().handlers(), store, WithLogger(quiet))
	if err := noReset.Rebuild(t.Context()); !errors.Is(err, ErrNoReset) {
		t.Errorf("Rebuild without WithReset = %v, want ErrNoReset", err)
	}
}

func TestRunner_VersionChangeRebuildsOnStart(t *testing.T) {
	store := memory.NewMemoryStore(0)
	head := appendTasks(t, store, 200)
	cps := &memoryCheckpoints{}

	c := newCounter()
	r1 := NewRunner("tasks", c.handlers(), store, WithCheckpoints(cps), WithReset(c.Reset), WithVersion(1), WithLogger(quiet))
	stop := run(t, r1)
	waitFor(t, r1, head)
	stop()

	// Same version: resumes, no reset.
	r2 := NewRunner("tasks", c.handlers(), store, WithCheckpoints(cps), WithReset(c.Reset), WithVersion(1), WithLogger(quiet))
	stop = run(t, r2)
	head = appendTasks(t, store, 20)
	waitFor(t, r2, head)
	stop()
	if _, _, resets := c.snapshot(); resets != 0 {
		t.Fatalf("resets after restart at the same version = %d, want 0", resets)
	}
	checkExactlyOnce(t, c, store)

	// New version: rebuilt from scratch.
	r3 := NewRunner("tasks", c.handlers(), store, WithCheckpoints(cps), WithReset(c.Reset), WithVersion(2), WithLogger(quiet))
	run(t, r3)
	waitFor(t, r3, head)
	if _, _, resets := c.snapshot(); resets != 1 {
		t.Fatalf("resets after version change = %d, want 1", resets)
	}
	checkExactlyOnce(t, c, store)
	if v := r3.Status().Version; v != 2 {
		t.Errorf("status version = %d, want 2", v)
	}
}

func fastRetry() backoff.BackOff {
	return backoff.NewConstantBackOff(10 * time.Millisecond)
}

func TestRunner_StallsOnFailingEventUntilFixed(t *testing.T) {
	store := memory.NewMemoryStore(0)
	head := appendTasks(t, store, 100)
	bad := handledPositions(t, store)[40]

	var broken atomic.Bool
	broken.Store(true)
	c := newCounter()
	c.fail = func(gv uint64) error {
		if gv == bad && broken.Load() {
			return errors.New("projector bug")
		}
		return nil
	}
	r := NewRunner("tasks", c.handlers(), store, WithCheckpoints(&txCheckpoints{}), WithRetryStrategy(fastRetry()), WithBatchSize(30), WithLogger(quiet))
	run(t, r)

	eventually(t, "stalled at the bad event", func() bool {
		s := r.Status()
		return s.Phase == Stalled && s.Position == bad-1
	})
	if s := r.Status(); !strings.Contains(s.LastError, fmt.Sprintf("event %d", bad)) || !strings.Contains(s.LastError, "projector bug") {
		t.Errorf("LastError = %q, want it to name event %d and the cause", s.LastError, bad)
	}

	broken.Store(false)
	waitFor(t, r, head)
	eventually(t, "live", func() bool { return r.Status().Phase == Live })
	checkExactlyOnce(t, c, store)
}

type deadLetterCheckpoints struct {
	txCheckpoints
	mu      sync.Mutex
	letters []uint64
}

func (d *deadLetterCheckpoints) SaveDeadLetter(_ context.Context, _ string, env *eventsourcing.Envelope, _ error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.letters = append(d.letters, env.GlobalVersion)
	return nil
}

func TestRunner_SkipAlwaysRecordsDeadLetters(t *testing.T) {
	store := memory.NewMemoryStore(0)
	head := appendTasks(t, store, 100)
	ps := handledPositions(t, store)
	bad := map[uint64]bool{ps[3]: true, ps[50]: true}

	c := newCounter()
	c.fail = func(gv uint64) error {
		if bad[gv] {
			return errors.New("poison")
		}
		return nil
	}
	cps := &deadLetterCheckpoints{}
	r := NewRunner("tasks", c.handlers(), store, WithCheckpoints(cps), WithErrorPolicy(SkipAlways), WithBatchSize(30), WithLogger(quiet))
	run(t, r)
	waitFor(t, r, head)

	if s := r.Status(); s.Skipped != 2 {
		t.Errorf("Skipped = %d, want 2", s.Skipped)
	}
	cps.mu.Lock()
	letters := cps.letters
	cps.mu.Unlock()
	if len(letters) != 2 || !bad[letters[0]] || !bad[letters[1]] {
		t.Errorf("dead letters = %v, want the two failing events", letters)
	}
	applied, _, _ := c.snapshot()
	for _, p := range ps {
		if want := 1; bad[p] {
			want = 0
			if applied[p] != want {
				t.Errorf("skipped event %d applied %d times", p, applied[p])
			}
		} else if applied[p] != want {
			t.Errorf("event %d applied %d times, want 1", p, applied[p])
		}
	}
}

func TestRunner_SkipWhenLiveStallsWhileCatchingUp(t *testing.T) {
	store := memory.NewMemoryStore(0)
	appendTasks(t, store, 60)
	bad := handledPositions(t, store)[10]
	c := newCounter()
	c.fail = func(gv uint64) error {
		if gv == bad {
			return errors.New("poison")
		}
		return nil
	}
	r := NewRunner("tasks", c.handlers(), store, WithErrorPolicy(SkipWhenLive), WithRetryStrategy(fastRetry()), WithBatchSize(20), WithLogger(quiet))
	run(t, r)
	eventually(t, "stalled while catching up", func() bool { return r.Status().Phase == Stalled })
	time.Sleep(50 * time.Millisecond)
	if s := r.Status(); s.Phase != Stalled || s.Skipped != 0 {
		t.Errorf("status = %+v, want stalled without skipping", s)
	}
}

func TestRunner_HooksRunAroundCatchUp(t *testing.T) {
	store := memory.NewMemoryStore(0)
	head := appendTasks(t, store, 500)
	var (
		mu    sync.Mutex
		calls []string
	)
	record := func(name string) func(context.Context, Status) error {
		return func(_ context.Context, s Status) error {
			mu.Lock()
			defer mu.Unlock()
			calls = append(calls, fmt.Sprintf("%s@%d", name, s.Position))
			return nil
		}
	}
	c := newCounter()
	r := NewRunner("tasks", c.handlers(), store, WithBatchSize(50),
		WithOnCatchingUp(record("catching_up")), WithOnLive(record("live")), WithLogger(quiet))
	run(t, r)
	waitFor(t, r, head)
	eventually(t, "live", func() bool { return r.Status().Phase == Live })

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 || calls[0] != "catching_up@0" || !strings.HasPrefix(calls[1], "live@") {
		t.Errorf("hook calls = %v, want [catching_up@0 live@...]", calls)
	}
}

func TestRunner_FailingOnCatchingUpStallsAndRetries(t *testing.T) {
	store := memory.NewMemoryStore(0)
	head := appendTasks(t, store, 200)
	var tries atomic.Int32
	c := newCounter()
	r := NewRunner("tasks", c.handlers(), store, WithBatchSize(50), WithRetryStrategy(fastRetry()), WithLogger(quiet),
		WithOnCatchingUp(func(context.Context, Status) error {
			if tries.Add(1) < 3 {
				return errors.New("cannot drop indexes")
			}
			return nil
		}))
	run(t, r)
	waitFor(t, r, head)
	if tries.Load() != 3 {
		t.Errorf("OnCatchingUp called %d times, want 3", tries.Load())
	}
	checkExactlyOnce(t, c, store)
}

// lockingCheckpoints adds an in-process Locker and StatusStore to the
// memory checkpoints, so several runners can share them like instances
// share a database.
type lockingCheckpoints struct {
	txCheckpoints
	mu     sync.Mutex
	holder *testLock
	status map[string]Status
}

type testLock struct {
	owner string
	lost  chan struct{}
	store *lockingCheckpoints
}

func (l *testLock) Lost() <-chan struct{} { return l.lost }
func (l *testLock) Owner() string         { return l.owner }
func (l *testLock) Release(context.Context) error {
	l.store.mu.Lock()
	defer l.store.mu.Unlock()
	if l.store.holder == l {
		l.store.holder = nil
	}
	return nil
}

var lockSeq atomic.Int32

func (s *lockingCheckpoints) TryLock(context.Context, string) (Lock, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.holder != nil {
		return nil, false, nil
	}
	s.holder = &testLock{owner: fmt.Sprintf("instance-%d", lockSeq.Add(1)), lost: make(chan struct{}), store: s}
	return s.holder, true, nil
}

func (s *lockingCheckpoints) breakLock() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.holder != nil {
		close(s.holder.lost)
		s.holder = nil
	}
}

func (s *lockingCheckpoints) SaveStatus(_ context.Context, name string, st Status) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status == nil {
		s.status = map[string]Status{}
	}
	cp, _ := s.Load(context.Background(), name)
	st.Position = cp.Position
	s.status[name] = st
	return nil
}

func (s *lockingCheckpoints) LoadStatus(_ context.Context, name string) (Status, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.status[name]
	if ok {
		cp, _ := s.Load(context.Background(), name)
		st.Position = cp.Position
	}
	return st, ok, nil
}

func TestRunner_OneActiveInstanceAndFailover(t *testing.T) {
	store := memory.NewMemoryStore(0)
	head := appendTasks(t, store, 400)
	cps := &lockingCheckpoints{}
	c := newCounter() // shared read model, like a database

	a := NewRunner("tasks", c.handlers(), store, WithCheckpoints(cps), WithReset(c.Reset), WithBatchSize(40), WithPollInterval(20*time.Millisecond), WithLogger(quiet))
	b := NewRunner("tasks", c.handlers(), store, WithCheckpoints(cps), WithReset(c.Reset), WithBatchSize(40), WithPollInterval(20*time.Millisecond), WithLogger(quiet))
	stopA := run(t, a)
	waitFor(t, a, head)
	run(t, b)

	eventually(t, "b in standby", func() bool { return b.Status().Phase == Standby })
	if err := b.Rebuild(t.Context()); !errors.Is(err, ErrNotActive) {
		t.Errorf("Rebuild on standby = %v, want ErrNotActive", err)
	}
	eventually(t, "b sees a live", func() bool { ok, _ := b.Live(t.Context()); return ok })

	stopA()
	head = appendTasks(t, store, 100)
	waitFor(t, b, head)
	eventually(t, "b active and live", func() bool { return b.Status().Phase == Live })
	checkExactlyOnce(t, c, store)

	// A lost lock stops the active runner, which competes again.
	cps.breakLock()
	head = appendTasks(t, store, 30)
	waitFor(t, b, head)
	checkExactlyOnce(t, c, store)
}

func TestRemote_LiveFollowsPublishedStatus(t *testing.T) {
	cps := &lockingCheckpoints{}
	dep := Remote("tasks", cps, MinVersion(2))
	if ok, _ := dep.Live(t.Context()); ok {
		t.Error("live without a published status")
	}
	_ = cps.SaveStatus(t.Context(), "tasks", Status{Phase: Live, Version: 1})
	if ok, _ := dep.Live(t.Context()); ok {
		t.Error("live below MinVersion")
	}
	_ = cps.SaveStatus(t.Context(), "tasks", Status{Phase: Live, Version: 2})
	if ok, _ := dep.Live(t.Context()); !ok {
		t.Error("not live with a live status at MinVersion")
	}
	if dep.Name() != "tasks" {
		t.Errorf("Name = %q", dep.Name())
	}
}
