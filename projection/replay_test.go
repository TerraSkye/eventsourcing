package projection_test

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/terraskye/eventsourcing/memory"
	"github.com/terraskye/eventsourcing/projection"
)

// newReplayRunner returns a runner of the two-segment projection "replay"
// for owner, which parks a stream whose handler fails.
func newReplayRunner(h *streamLog, es *testStore, ps projection.Store, owner string) *projection.Runner {
	return projection.NewRunner(projection.Definition{
		Name:         "replay",
		Handler:      h,
		EventTypes:   []string{"projection_test.TaskCreated", "projection_test.TaskRenamed"},
		OnError:      projection.SkipAlways,
		Owner:        owner,
		Segments:     2,
		SegmentKey:   projection.ByStreamID,
		PollInterval: 10 * time.Millisecond,
		ClaimTTL:     time.Second,
	}, es, ps)
}

// parkOnTwoInstances runs instances a and b of "replay", one segment each,
// until s1's events are dead letters, and returns them with s1's segment.
func parkOnTwoInstances(t *testing.T, h *streamLog, es *testStore, ps *memory.ProjectionStore) (a, b *projection.Runner, seg int) {
	t.Helper()
	last := parkS1(t, es)
	h.failing.Store(true)
	a = newReplayRunner(h, es, ps, "a")
	b = newReplayRunner(h, es, ps, "b")
	t.Cleanup(runRunner(t, a))
	t.Cleanup(runRunner(t, b))
	waitHeldBy(t, ps, "replay", map[string]int{a.Owner(): 1, b.Owner(): 1}, 3*time.Second)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := a.WaitUntil(ctx, last); err != nil {
		t.Fatalf("WaitUntil(%d): %v", last, err)
	}
	seg = projection.SegmentOf("s1", 2)
	if letters, err := ps.LoadDeadLetters(t.Context(), "replay", seg); err != nil || len(letters) == 0 {
		t.Fatalf("dead letters of s1 = %+v, %v; want s1 parked", letters, err)
	}
	return a, b, seg
}

// waitHeldBy waits until the segments of name are held exactly as want
// says, by full Owner.
func waitHeldBy(t *testing.T, ps projection.Store, name string, want map[string]int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		sts, err := ps.LoadStatuses(t.Context(), name)
		if err != nil {
			t.Fatalf("LoadStatuses: %v", err)
		}
		got := map[string]int{}
		for _, st := range sts {
			got[st.Owner]++
		}
		if len(got) == len(want) && !slices.ContainsFunc(sts, func(st projection.Status) bool { return got[st.Owner] != want[st.Owner] }) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("segments held %v after %s, want %v", got, timeout, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestRunner_ReplayStreamFromAnyProcess covers a replay requested by a
// runner that is not running, as an admin tool would: the instance holding
// the parked stream's segment serves it, and ReplayStream returns once it
// did, with the stream's events applied and its dead letters gone.
func TestRunner_ReplayStreamFromAnyProcess(t *testing.T) {
	es, ps, h := &testStore{}, memory.NewProjectionStore(), &streamLog{}
	_, _, seg := parkOnTwoInstances(t, h, es, ps)
	h.failing.Store(false)

	admin := newReplayRunner(h, es, ps, "admin") // never Run
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := admin.ReplayStream(ctx, "s1"); err != nil {
		t.Fatalf("ReplayStream: %v", err)
	}
	if letters, err := ps.LoadDeadLetters(t.Context(), "replay", seg); err != nil || len(letters) != 0 {
		t.Fatalf("dead letters of s1 after ReplayStream = %+v, %v; want none", letters, err)
	}
	if got := h.snapshot(); !slices.Contains(got, "s1:projection_test.TaskCreated") {
		t.Fatalf("applied %v after ReplayStream, want s1's failed event among them", got)
	}
}

// TestRunner_ReplayStreamReportsRemoteError covers a replay called on the
// instance that does not hold the stream's segment, while the handler
// still fails: the instance that holds it tries, and the call returns the
// error it failed with.
func TestRunner_ReplayStreamReportsRemoteError(t *testing.T) {
	es, ps, h := &testStore{}, memory.NewProjectionStore(), &streamLog{}
	a, b, seg := parkOnTwoInstances(t, h, es, ps)
	sts, err := ps.LoadStatuses(t.Context(), "replay")
	if err != nil {
		t.Fatalf("LoadStatuses: %v", err)
	}
	caller := a
	if sts[seg].Owner == a.Owner() {
		caller = b
	}

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	err = caller.ReplayStream(ctx, "s1")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("ReplayStream on the instance without s1's segment = %v, want the handler's error from the other one", err)
	}
	if letters, err := ps.LoadDeadLetters(t.Context(), "replay", seg); err != nil || len(letters) == 0 {
		t.Fatalf("dead letters of s1 after a failed replay = %+v, %v; want s1 still parked", letters, err)
	}
}

// TestRunner_ReplayStreamKeepsLocalError covers a replay served on the
// instance that called ReplayStream, while the handler still fails: the
// call returns the handler's error itself, not only its text.
func TestRunner_ReplayStreamKeepsLocalError(t *testing.T) {
	es, ps, h := &testStore{}, memory.NewProjectionStore(), &streamLog{}
	a, b, seg := parkOnTwoInstances(t, h, es, ps)
	sts, err := ps.LoadStatuses(t.Context(), "replay")
	if err != nil {
		t.Fatalf("LoadStatuses: %v", err)
	}
	caller := a
	if sts[seg].Owner == b.Owner() {
		caller = b
	}

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := caller.ReplayStream(ctx, "s1"); !errors.Is(err, errBoom) {
		t.Fatalf("ReplayStream on the instance with s1's segment = %v, want errBoom", err)
	}
}

// TestRunner_ReplayStreamNothingToReplay covers a stream without dead
// letters: ReplayStream returns nil at once, even with no instance running.
func TestRunner_ReplayStreamNothingToReplay(t *testing.T) {
	ps := memory.NewProjectionStore()
	if err := ps.Declare(t.Context(), "replay", 2, 0); err != nil {
		t.Fatalf("Declare: %v", err)
	}
	r := newReplayRunner(&streamLog{}, &testStore{}, ps, "a")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := r.ReplayStream(ctx, "s9"); err != nil {
		t.Fatalf("ReplayStream of a stream without dead letters = %v, want nil", err)
	}
}

// pollStore counts PendingReplays calls, and fails them while fail is set.
type pollStore struct {
	*memory.ProjectionStore
	calls atomic.Int32
	fail  atomic.Bool
}

func (s *pollStore) PendingReplays(ctx context.Context, name string) ([]projection.Replay, error) {
	s.calls.Add(1)
	if s.fail.Load() {
		return nil, errors.New("connection reset")
	}
	return s.ProjectionStore.PendingReplays(ctx, name)
}

// TestRunner_ReplayPollIdleWithoutDeadLetters covers a projection whose
// segments hold no dead letters: once its segments are claimed, the runner
// stops reading replay requests.
func TestRunner_ReplayPollIdleWithoutDeadLetters(t *testing.T) {
	es, ps := &testStore{}, &pollStore{ProjectionStore: memory.NewProjectionStore()}
	save(t, es, 3)
	r := newReplayRunner(&streamLog{}, es, ps, "a")
	defer runRunner(t, r)()
	waitHeldBy(t, ps, "replay", map[string]int{r.Owner(): 2}, 2*time.Second)

	time.Sleep(5 * 10 * time.Millisecond)
	before := ps.calls.Load()
	time.Sleep(20 * 10 * time.Millisecond)
	if n := ps.calls.Load() - before; n != 0 {
		t.Fatalf("PendingReplays called %d times in 20 poll intervals without dead letters, want 0", n)
	}
}

// TestRunner_ReplayPollLogsFailure covers a failing read of the replay
// requests: it is logged as one warning however often it fails, and its
// recovery once.
func TestRunner_ReplayPollLogsFailure(t *testing.T) {
	es, ps := &testStore{}, &pollStore{ProjectionStore: memory.NewProjectionStore()}
	ps.fail.Store(true)
	logs := &logRecorder{}
	r := projection.NewRunner(projection.Definition{
		Name:         "replay",
		Handler:      &streamLog{},
		EventTypes:   []string{"projection_test.TaskCreated", "projection_test.TaskRenamed"},
		Owner:        "a",
		Segments:     2,
		SegmentKey:   projection.ByStreamID,
		Logger:       slog.New(logs),
		PollInterval: 10 * time.Millisecond,
		ClaimTTL:     time.Second,
	}, es, ps)
	defer runRunner(t, r)()
	const warning = "projection replay poll failed; requested replays wait"
	waitLog(t, logs, warning, 2*time.Second)
	for ps.calls.Load() < 5 {
		time.Sleep(5 * time.Millisecond)
	}
	ps.fail.Store(false)
	deadline := time.Now().Add(2 * time.Second)
	for !slices.ContainsFunc(logs.snapshot(), func(r slog.Record) bool { return r.Message == "projection replay poll recovered" }) {
		if time.Now().After(deadline) {
			t.Fatal("no recovery logged after PendingReplays succeeded again")
		}
		time.Sleep(5 * time.Millisecond)
	}
	warned := 0
	for _, r := range logs.snapshot() {
		if r.Message == warning {
			warned++
		}
	}
	if warned != 1 {
		t.Fatalf("logged %d warnings for %d failed polls, want 1", warned, ps.calls.Load()-1)
	}
}
