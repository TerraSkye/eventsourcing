package projection_test

import (
	"context"
	"errors"
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

// TestRunner_StallBackoffGrows covers a handler that keeps failing under
// StallOnError: the waits between retries grow from 0.5s towards 30s,
// although every retry claims the segment again.
func TestRunner_StallBackoffGrows(t *testing.T) {
	es := &testStore{}
	ps := memory.NewProjectionStore()
	if _, err := es.Save(t.Context(), []eventsourcing.Envelope{
		{EventID: uuid.New(), StreamID: "s", Event: &taskCreated{ID: "s"}, Version: 1},
	}, eventsourcing.Any{}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	var mu sync.Mutex
	var attempts []time.Time
	h := eventsourcing.NewEventHandlerFunc(func(context.Context, eventsourcing.Event) error {
		mu.Lock()
		defer mu.Unlock()
		now := time.Now()
		// The batch and its one-by-one retry are one attempt.
		if n := len(attempts); n == 0 || now.Sub(attempts[n-1]) > 100*time.Millisecond {
			attempts = append(attempts, now)
		}
		return errors.New("always fails")
	})
	r := projection.NewRunner(projection.Definition{
		Name:         "p",
		Owner:        "a",
		Handler:      h,
		EventTypes:   counterEvents,
		PollInterval: 10 * time.Millisecond,
		ClaimTTL:     time.Minute,
	}, es, ps)
	defer runRunner(t, r)()

	deadline := time.After(10 * time.Second)
	for {
		mu.Lock()
		n := len(attempts)
		mu.Unlock()
		if n >= 4 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("only %d attempts in 10s", n)
		case <-time.After(10 * time.Millisecond):
		}
	}
	mu.Lock()
	defer mu.Unlock()
	var gaps []time.Duration
	for i := 1; i < 4; i++ {
		gaps = append(gaps, attempts[i].Sub(attempts[i-1]))
	}
	// 0.5s, 1s, 2s with ±50% jitter: the third wait is at least 1s,
	// while a 0.5s wait that never grows is at most 0.75s.
	if gaps[2] < 900*time.Millisecond {
		t.Fatalf("stall backoff does not grow: waits between attempts %v", gaps)
	}
}

// claimErrStore fails the first fails calls to Claim, as a store with a
// transient connection error would.
type claimErrStore struct {
	projection.Store
	mu    sync.Mutex
	fails int
}

func (s *claimErrStore) Claim(ctx context.Context, l projection.Lease, ttl time.Duration) (projection.Lease, projection.Checkpoint, bool, error) {
	s.mu.Lock()
	fail := s.fails > 0
	if fail {
		s.fails--
	}
	s.mu.Unlock()
	if fail {
		return projection.Lease{}, projection.Checkpoint{}, false, errors.New("connection reset")
	}
	return s.Store.Claim(ctx, l, ttl)
}

// Heartbeat reports owner as the only instance, so its runner competes for
// every segment, as it does while the other instances' heartbeats are late.
func (s *claimErrStore) Heartbeat(ctx context.Context, name, owner string, version int, ttl time.Duration) ([]projection.Instance, error) {
	instances, err := s.Store.Heartbeat(ctx, name, owner, version, ttl)
	return slices.DeleteFunc(instances, func(m projection.Instance) bool { return m.Owner != owner }), err
}

// TestRunner_ClaimErrorLeavesOwnerToStore covers a Runner whose Claim fails
// with a store error while another instance holds the segment: it does not
// report itself as the owner, so IsLive and WaitUntil ask the store, where
// the real owner's progress is.
func TestRunner_ClaimErrorLeavesOwnerToStore(t *testing.T) {
	newDef := func(owner string, segments int) projection.Definition {
		d := projection.Definition{
			Name:         "test",
			Handler:      eventsourcing.NewEventHandlerFunc(newCounter().Handle),
			EventTypes:   counterEvents,
			Owner:        owner,
			PollInterval: 10 * time.Millisecond,
			ClaimTTL:     time.Second,
		}
		if segments > 1 {
			d.Segments, d.SegmentKey = segments, projection.ByStreamID
		}
		return d
	}

	tests := []struct {
		name     string
		segments int
	}{
		{name: "one segment", segments: 1},
		{name: "two segments", segments: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			es := &testStore{}
			ps := memory.NewProjectionStore()

			// b owns every segment and processes the log.
			b := projection.NewRunner(newDef("b", tt.segments), es, ps)
			defer runRunner(t, b)()
			pos := save(t, es, 3)
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			if err := b.WaitUntil(ctx, pos); err != nil {
				t.Fatalf("b.WaitUntil(%d): %v", pos, err)
			}

			// a's first Claim on each segment fails with a store error.
			as := &claimErrStore{Store: ps, fails: tt.segments}
			a := projection.NewRunner(newDef("a", tt.segments), es, as)
			defer runRunner(t, a)()
			for a.Status().Phase != projection.Stalled {
				if ctx.Err() != nil {
					t.Fatal("a never stalled on its failing Claim")
				}
				time.Sleep(time.Millisecond)
			}

			if st := a.Status(); st.Owner == "a" {
				t.Errorf("a.Status().Owner = %q after a Claim error, while b holds every segment; Status() = %+v", st.Owner, st)
			}
			if tt.segments == 1 {
				live, err := a.IsLive(t.Context())
				if err != nil || !live {
					t.Errorf("a.IsLive() = %v, %v; want true, nil: b owns the segment and is live", live, err)
				}
			}
			// 200ms is 20 PollIntervals, well before a's retry backoff ends.
			wctx, wcancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer wcancel()
			if err := a.WaitUntil(wctx, pos); err != nil {
				t.Errorf("a.WaitUntil(%d) = %v; b already processed it", pos, err)
			}
		})
	}
}

// TestRunner_NotOwnerAfterRunReturns covers a Runner whose Run has returned,
// and so released every claim: IsLive and WaitUntil answer from the store
// rather than from its last in-memory status.
func TestRunner_NotOwnerAfterRunReturns(t *testing.T) {
	newDef := func(owner string) projection.Definition {
		return projection.Definition{
			Name:         "test",
			Handler:      eventsourcing.NewEventHandlerFunc(newCounter().Handle),
			EventTypes:   counterEvents,
			Owner:        owner,
			PollInterval: 10 * time.Millisecond,
			ClaimTTL:     time.Second,
		}
	}
	// startAndStop runs a until it is live, then stops it.
	startAndStop := func(t *testing.T, a *projection.Runner) {
		t.Helper()
		stop := runRunner(t, a)
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		for {
			if live, _ := a.IsLive(ctx); live {
				break
			}
			if ctx.Err() != nil {
				t.Fatal("runner never went live")
			}
			time.Sleep(10 * time.Millisecond)
		}
		stop() // Run has returned and released the claim
	}

	tests := []struct {
		name  string
		check func(t *testing.T, es *testStore, ps *memory.ProjectionStore, a *projection.Runner)
	}{
		{
			name: "IsLive with an unprocessed event and no owner",
			check: func(t *testing.T, es *testStore, ps *memory.ProjectionStore, a *projection.Runner) {
				pos := save(t, es, 1)
				live, err := a.IsLive(t.Context())
				if err != nil {
					t.Fatalf("IsLive: %v", err)
				}
				if live {
					t.Errorf("IsLive() = true after Run returned, with event %d unprocessed by anyone; Status() = %+v", pos, a.Status())
				}
			},
		},
		{
			name: "WaitUntil for a position another instance reached",
			check: func(t *testing.T, es *testStore, ps *memory.ProjectionStore, a *projection.Runner) {
				pos := save(t, es, 1)
				b := projection.NewRunner(newDef("b"), es, ps)
				defer runRunner(t, b)()
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				defer cancel()
				if err := b.WaitUntil(ctx, pos); err != nil {
					t.Fatalf("b.WaitUntil(%d): %v", pos, err)
				}
				ctx, cancel = context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				if err := a.WaitUntil(ctx, pos); err != nil {
					t.Errorf("a.WaitUntil(%d) = %v after b processed it; a.Status() = %+v", pos, err, a.Status())
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			es := &testStore{}
			ps := memory.NewProjectionStore()
			a := projection.NewRunner(newDef("a"), es, ps)
			startAndStop(t, a)
			tt.check(t, es, ps, a)
		})
	}
}

type oneByOneUnhandled struct{}

func (oneByOneUnhandled) AggregateID() string { return "other" }
func (oneByOneUnhandled) EventType() string   { return "projection_test.OneByOneUnhandled" }

// TestRunner_OneByOneRetryCommitsScannedPosition covers a batch whose
// handler fails once and succeeds when the runner retries it event by
// event, where the read also scanned events the projection does not handle
// past the last handled one. The store's checkpoint moves to the end of the
// read too, so a wait on the store sees the scanned position.
func TestRunner_OneByOneRetryCommitsScannedPosition(t *testing.T) {
	// Position 2 is filtered out by EventTypes; the read scans it after the
	// failing event.
	envs := []eventsourcing.Envelope{
		{EventID: uuid.New(), StreamID: "s", Event: &taskCreated{ID: "s"}, Version: 1},
		{EventID: uuid.New(), StreamID: "other", Event: oneByOneUnhandled{}, Version: 1},
	}
	es := &testStore{}
	ps := memory.NewProjectionStore()
	res, err := es.Save(t.Context(), envs, eventsourcing.Any{})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	last := res.GlobalPosition

	// The first batch fails; the event-by-event retry succeeds.
	var calls atomic.Int32
	h := eventsourcing.NewEventHandlerFunc(func(context.Context, eventsourcing.Event) error {
		if calls.Add(1) == 1 {
			return errors.New("transient")
		}
		return nil
	})
	r := projection.NewRunner(projection.Definition{
		Name:         "p",
		Owner:        "a",
		Handler:      h,
		EventTypes:   counterEvents,
		PollInterval: 10 * time.Millisecond,
		ClaimTTL:     time.Second,
	}, es, ps)
	defer runRunner(t, r)()

	// The runner itself gets past the whole read.
	deadline := time.After(2 * time.Second)
	for r.Status().Position < last {
		select {
		case <-deadline:
			t.Fatalf("runner never reached %d in memory: %+v", last, r.Status())
		case <-time.After(5 * time.Millisecond):
		}
	}

	// Every event up to last is processed, so a wait on the store
	// must return within a few poll intervals.
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	probe := projection.NewProbe(ps, "p", projection.WithPollInterval(5*time.Millisecond))
	if err := probe.WaitUntil(ctx, last); err != nil {
		statuses, _ := ps.LoadStatuses(t.Context(), "p")
		t.Fatalf("Probe.WaitUntil(%d) = %v\nin memory: %+v\nin store: %+v", last, err, r.Status(), statuses)
	}
}

// replayDroppedGateReader blocks the first ReadAll after armed is set
// until release is closed, so ReplayStream requests can queue up.
type replayDroppedGateReader struct {
	*testStore
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (g *replayDroppedGateReader) ReadAll(ctx context.Context, after uint64, opts eventsourcing.ReadOptions) (eventsourcing.ReadResult, error) {
	if g.armed.CompareAndSwap(true, false) {
		close(g.entered)
		<-g.release
	}
	return g.testStore.ReadAll(ctx, after, opts)
}

// replayDroppedLosingStore fails the next Commit with ErrClaimLost once
// lose is set, as when another instance took the segment.
type replayDroppedLosingStore struct {
	*memory.ProjectionStore
	lose atomic.Bool
}

func (s *replayDroppedLosingStore) Commit(ctx context.Context, l projection.Lease, cp projection.Checkpoint, status projection.Status, fn func(ctx context.Context) error) error {
	if s.lose.CompareAndSwap(true, false) {
		return projection.ErrClaimLost
	}
	return s.ProjectionStore.Commit(ctx, l, cp, status, fn)
}

// TestRunner_ReplayRequestsFinishAfterClaimLost covers that every queued
// ReplayStream call returns when one replay loses the claim, instead of
// the ones behind it waiting until their ctx ends.
func TestRunner_ReplayRequestsFinishAfterClaimLost(t *testing.T) {
	es := &testStore{}
	var last uint64
	for _, e := range []eventsourcing.Event{&taskCreated{ID: "1"}, &taskCreated{ID: "2"}} {
		res, err := es.Save(t.Context(), []eventsourcing.Envelope{{EventID: uuid.New(), StreamID: "s1", Event: e}}, eventsourcing.Any{})
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		last = res.GlobalPosition
	}
	reader := &replayDroppedGateReader{testStore: es, entered: make(chan struct{}), release: make(chan struct{})}
	ps := &replayDroppedLosingStore{ProjectionStore: memory.NewProjectionStore()}
	h := &streamLog{}
	h.failing.Store(true) // s1's events become dead letters
	r := projection.NewRunner(projection.Definition{
		Name:         "test",
		Handler:      h,
		EventTypes:   []string{"projection_test.TaskCreated"},
		OnError:      projection.SkipAlways,
		PollInterval: 10 * time.Millisecond,
		ClaimTTL:     time.Second,
	}, reader, ps)
	defer runRunner(t, r)()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := r.WaitUntil(ctx, last); err != nil {
		t.Fatalf("WaitUntil: %v", err)
	}
	h.failing.Store(false)

	// Hold the segment in a read, queue two replays for it, then make the
	// first replay's commit lose the claim.
	reader.armed.Store(true)
	select {
	case <-reader.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("runner never read")
	}
	results := make(chan error, 2)
	for range 2 {
		go func() { results <- r.ReplayStream(ctx, "s1") }()
	}
	time.Sleep(200 * time.Millisecond) // let both requests queue
	ps.lose.Store(true)
	close(reader.release)

	for i := range 2 {
		select {
		case err := <-results:
			t.Logf("ReplayStream call %d returned %v", i+1, err)
		case <-time.After(2 * time.Second):
			t.Fatalf("ReplayStream call %d has not returned 2s after the claim was lost and the segment reclaimed; it waits until its ctx ends", i+1)
		}
	}
}

// flakyReclaimStore fails the first Claim after a successful Reset, as a
// store with a transient connection error would.
type flakyReclaimStore struct {
	projection.Store
	mu       sync.Mutex
	failNext bool
}

func (s *flakyReclaimStore) Reset(ctx context.Context, l projection.Lease, v int, wipe func(context.Context) error) error {
	err := s.Store.Reset(ctx, l, v, wipe)
	if err == nil {
		s.mu.Lock()
		s.failNext = true
		s.mu.Unlock()
	}
	return err
}

func (s *flakyReclaimStore) Claim(ctx context.Context, l projection.Lease, ttl time.Duration) (projection.Lease, projection.Checkpoint, bool, error) {
	s.mu.Lock()
	fail := s.failNext
	s.failNext = false
	s.mu.Unlock()
	if fail {
		return projection.Lease{}, projection.Checkpoint{}, false, errors.New("transient claim failure")
	}
	return s.Store.Claim(ctx, l, ttl)
}

// TestRunner_RebuildFailedReclaimResetsPositions covers a rebuild whose
// Store.Reset commits but whose reclaim then fails: WaitUntil after Rebuild
// waits for the replay instead of seeing the positions from before the
// wipe.
func TestRunner_RebuildFailedReclaimResetsPositions(t *testing.T) {
	for _, tc := range []struct {
		name     string
		segments int
	}{
		{"single segment", 1},
		{"two segments", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			es := &testStore{}
			c := newCounter()
			ps := &flakyReclaimStore{Store: memory.NewProjectionStore()}
			p := projection.Definition{
				Name:         "test",
				Handler:      eventsourcing.NewEventHandlerFunc(c.Handle),
				EventTypes:   counterEvents,
				Reset:        c.Reset,
				Version:      1,
				PollInterval: 10 * time.Millisecond,
				ClaimTTL:     time.Second,
			}
			if tc.segments > 1 {
				p.Segments, p.SegmentKey = tc.segments, projection.ByStreamID
			}
			r := projection.NewRunner(p, es, ps)
			last := save(t, es, 5)
			stop := runRunner(t, r)
			defer stop()

			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			if err := r.WaitUntil(ctx, last); err != nil {
				t.Fatalf("WaitUntil before rebuild: %v", err)
			}
			if err := r.Rebuild(ctx); err != nil {
				t.Fatalf("Rebuild = %v", err)
			}
			if err := r.WaitUntil(ctx, last); err != nil {
				t.Fatalf("WaitUntil after rebuild: %v", err)
			}
			c.mu.Lock()
			n := len(c.applied)
			c.mu.Unlock()
			if n != 5 {
				t.Errorf("after Rebuild, WaitUntil(%d) returned with %d of 5 events applied to the wiped read model", last, n)
			}
		})
	}
}

// TestRunner_PublishesStall covers that a stall reaches the store: while a
// segment retries, it still holds its claim, so without publishing other
// instances would keep seeing the phase of its last commit. A Probe waiting
// past the stall reports why, and sees it clear once the runner recovers.
func TestRunner_PublishesStall(t *testing.T) {
	es := &testStore{}
	ps := memory.NewProjectionStore()

	var failing atomic.Bool
	failing.Store(true)
	c := newCounter()
	c.fail = func(gv uint64) error {
		if gv == 2 && failing.Load() {
			return errors.New("boom")
		}
		return nil
	}
	save(t, es, 1)
	r := newSharedRunner(c, es, ps)
	defer runRunner(t, r)()

	probe := fastProbe(ps)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := probe.WaitUntil(ctx, 1); err != nil {
		t.Fatalf("WaitUntil(1): %v", err)
	}
	last := save(t, es, 1)

	for {
		statuses, err := ps.LoadStatuses(ctx, "test")
		if err != nil {
			t.Fatalf("LoadStatuses: %v", err)
		}
		if len(statuses) == 1 && statuses[0].Phase == projection.Stalled {
			if !strings.Contains(statuses[0].LastError, "boom") {
				t.Fatalf("stored LastError = %q, want the handler's error", statuses[0].LastError)
			}
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("stored status never showed the stall; last %+v", statuses)
		}
		time.Sleep(5 * time.Millisecond)
	}

	short, cancelShort := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancelShort()
	err := probe.WaitUntil(short, last)
	var nr *projection.NotReachedError
	if !errors.As(err, &nr) {
		t.Fatalf("WaitUntil past the stall = %v, want a *NotReachedError", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "stalled") || !strings.Contains(msg, "boom") {
		t.Errorf("Error() = %q, want it to name the stall and its cause", msg)
	}

	failing.Store(false)
	if err := probe.WaitUntil(ctx, last); err != nil {
		t.Fatalf("WaitUntil after recovery: %v", err)
	}
	statuses, err := ps.LoadStatuses(ctx, "test")
	if err != nil {
		t.Fatalf("LoadStatuses: %v", err)
	}
	if statuses[0].LastError != "" {
		t.Errorf("stored LastError = %q after recovery, want empty", statuses[0].LastError)
	}
}

// soloStore reports each runner as the only live instance, so it competes
// for every segment, as it does while the other instances' heartbeats are
// late.
type soloStore struct{ projection.Store }

func (s soloStore) Heartbeat(ctx context.Context, name, owner string, version int, ttl time.Duration) ([]projection.Instance, error) {
	instances, err := s.Store.Heartbeat(ctx, name, owner, version, ttl)
	return slices.DeleteFunc(instances, func(m projection.Instance) bool { return m.Owner != owner }), err
}

// TestRunner_StallKeepsClaim covers a segment stalled for longer than its
// ClaimTTL: it keeps refreshing its claim during the backoff, so another
// instance does not take it over only to stall on the same event.
func TestRunner_StallKeepsClaim(t *testing.T) {
	es := &testStore{}
	ps := memory.NewProjectionStore()
	failing := newCounter()
	failing.fail = func(uint64) error { return errors.New("poison event") }
	newDef := func(c *counter, owner string) projection.Definition {
		return projection.Definition{
			Name:         "test",
			Handler:      eventsourcing.NewEventHandlerFunc(c.Handle),
			EventTypes:   counterEvents,
			Owner:        owner,
			PollInterval: 20 * time.Millisecond,
			// Below the first backoff of 0.5s, rounded down by jitter.
			ClaimTTL: 200 * time.Millisecond,
		}
	}
	save(t, es, 1)
	a := projection.NewRunner(newDef(failing, "a"), es, ps)
	defer runRunner(t, a)()
	for a.Status().Phase != projection.Stalled {
		if t.Context().Err() != nil {
			t.Fatal("a never stalled")
		}
		time.Sleep(5 * time.Millisecond)
	}

	b := projection.NewRunner(newDef(newCounter(), "b"), es, soloStore{ps})
	defer runRunner(t, b)()
	// Well past a's first backoff and several ClaimTTLs.
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		sts, err := ps.LoadStatuses(t.Context(), "test")
		if err != nil {
			t.Fatalf("LoadStatuses: %v", err)
		}
		if owner := sts[0].Owner; owner != "a" {
			t.Fatalf("segment owned by %q while a was stalled on it, want a to keep its claim", owner)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
