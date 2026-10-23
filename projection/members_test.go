package projection_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/memory"
	"github.com/terraskye/eventsourcing/projection"
)

// newSpreadRunner returns a runner of the segmented projection "spread" for
// owner, polling fast so segments settle within a test.
func newSpreadRunner(es *testStore, ps projection.Store, owner string, segments, version int) *projection.Runner {
	c := newCounter()
	p := projection.Definition{
		Name:         "spread",
		Handler:      eventsourcing.NewEventHandlerFunc(c.Handle),
		EventTypes:   counterEvents,
		Owner:        owner,
		Segments:     segments,
		SegmentKey:   projection.ByStreamID,
		PollInterval: 10 * time.Millisecond,
		ClaimTTL:     time.Second,
	}
	if version > 0 {
		p.Reset, p.Version = c.Reset, version
	}
	return projection.NewRunner(p, es, ps)
}

// held returns how many segments of "spread" each owner holds, and how
// many no one holds.
func held(t *testing.T, ps projection.Store) (map[string]int, int) {
	t.Helper()
	sts, err := ps.LoadStatuses(t.Context(), "spread")
	if err != nil {
		t.Fatalf("LoadStatuses: %v", err)
	}
	counts, free := map[string]int{}, 0
	for _, st := range sts {
		if st.Owner == "" {
			free++
			continue
		}
		counts[st.Owner]++
	}
	return counts, free
}

// waitHeld waits until the segments are held exactly as want says, with
// none free, and fails the test after timeout.
func waitHeld(t *testing.T, ps projection.Store, want map[string]int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		got, free := held(t, ps)
		if free == 0 && fmt.Sprint(got) == fmt.Sprint(want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("segments held %v with %d free after %s, want %v", got, free, timeout, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitSpread waits until every owner holds segments/len(owners) segments,
// rounded down or up, with none free.
func waitSpread(t *testing.T, ps projection.Store, owners []string, segments int, timeout time.Duration) {
	t.Helper()
	lo := segments / len(owners)
	hi := lo
	if segments%len(owners) != 0 {
		hi++
	}
	deadline := time.Now().Add(timeout)
	for {
		got, free := held(t, ps)
		spread := free == 0
		for _, o := range owners {
			spread = spread && got[o] >= lo && got[o] <= hi
		}
		if spread {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("segments held %v with %d free after %s, want %d to %d for each of %v", got, free, timeout, lo, hi, owners)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestRunner_SpreadsSegments covers instances started together: each ends
// up with floor(S/N) or ceil(S/N) segments, also where a cap of ceil(S/N)
// alone would leave one at 0 (S=4, N=3).
func TestRunner_SpreadsSegments(t *testing.T) {
	tests := []struct {
		segments  int
		instances int
	}{
		{segments: 8, instances: 2},
		{segments: 8, instances: 3},
		{segments: 4, instances: 3},
		{segments: 2, instances: 3},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("S=%d,N=%d", tt.segments, tt.instances), func(t *testing.T) {
			es, ps := &testStore{}, memory.NewProjectionStore()
			save(t, es, 5)
			var owners []string
			for i := range tt.instances {
				owner := fmt.Sprintf("w%d", i)
				owners = append(owners, owner)
				defer runRunner(t, newSpreadRunner(es, ps, owner, tt.segments, 0))()
			}
			waitSpread(t, ps, owners, tt.segments, 3*time.Second)
		})
	}
}

// TestRunner_SingleInstanceHoldsEverySegment covers one instance alone: it
// claims every segment, as before segments were spread.
func TestRunner_SingleInstanceHoldsEverySegment(t *testing.T) {
	es, ps := &testStore{}, memory.NewProjectionStore()
	pos := save(t, es, 5)
	r := newSpreadRunner(es, ps, "a", 4, 0)
	defer runRunner(t, r)()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := r.WaitUntil(ctx, pos); err != nil {
		t.Fatalf("WaitUntil(%d): %v", pos, err)
	}
	waitHeld(t, ps, map[string]int{"a": 4}, time.Second)
}

// TestRunner_JoinTakesShare covers an instance that starts while another
// holds every segment: it gets its share without the other stopping.
func TestRunner_JoinTakesShare(t *testing.T) {
	es, ps := &testStore{}, memory.NewProjectionStore()
	save(t, es, 5)
	defer runRunner(t, newSpreadRunner(es, ps, "a", 8, 0))()
	waitHeld(t, ps, map[string]int{"a": 8}, 2*time.Second)

	defer runRunner(t, newSpreadRunner(es, ps, "b", 8, 0))()
	waitHeld(t, ps, map[string]int{"a": 4, "b": 4}, 3*time.Second)
}

// TestRunner_JoinKeepsSingleSegment covers a second instance of an
// unsegmented projection: the segment stays where it is, whichever owner
// sorts first.
func TestRunner_JoinKeepsSingleSegment(t *testing.T) {
	es, ps := &testStore{}, memory.NewProjectionStore()
	save(t, es, 5)
	defer runRunner(t, newSpreadRunner(es, ps, "b", 1, 0))()
	waitHeld(t, ps, map[string]int{"b": 1}, 2*time.Second)

	defer runRunner(t, newSpreadRunner(es, ps, "a", 1, 0))()
	// Several heartbeats past the grace in which "a" could have taken it.
	time.Sleep(200 * time.Millisecond)
	waitHeld(t, ps, map[string]int{"b": 1}, 0)
}

// TestRunner_StopHandsShareOver covers an instance that stops: it leaves
// the members at once, so the rest take its segments without waiting out
// ClaimTTL.
func TestRunner_StopHandsShareOver(t *testing.T) {
	es, ps := &testStore{}, memory.NewProjectionStore()
	save(t, es, 5)
	defer runRunner(t, newSpreadRunner(es, ps, "a", 6, 0))()
	defer runRunner(t, newSpreadRunner(es, ps, "b", 6, 0))()
	stopC := runRunner(t, newSpreadRunner(es, ps, "c", 6, 0))
	waitSpread(t, ps, []string{"a", "b", "c"}, 6, 3*time.Second)

	stopC()
	// Well under the 1s ClaimTTL a crashed instance leaves behind.
	waitHeld(t, ps, map[string]int{"a": 3, "b": 3}, 500*time.Millisecond)
}

// deadStore fails every call once killed, as a store would see an instance
// that crashed: it neither refreshes nor releases anything.
type deadStore struct {
	projection.Store
	dead atomic.Bool
}

var errDead = errors.New("instance crashed")

func (s *deadStore) Claim(ctx context.Context, l projection.Lease, ttl time.Duration) (projection.Lease, projection.Checkpoint, bool, error) {
	if s.dead.Load() {
		return projection.Lease{}, projection.Checkpoint{}, false, errDead
	}
	return s.Store.Claim(ctx, l, ttl)
}

func (s *deadStore) Commit(ctx context.Context, l projection.Lease, cp projection.Checkpoint, status projection.Status, fn func(ctx context.Context) error) error {
	if s.dead.Load() {
		return errDead
	}
	return s.Store.Commit(ctx, l, cp, status, fn)
}

func (s *deadStore) SaveStatus(ctx context.Context, l projection.Lease, status projection.Status) error {
	if s.dead.Load() {
		return errDead
	}
	return s.Store.SaveStatus(ctx, l, status)
}

func (s *deadStore) Release(ctx context.Context, l projection.Lease) error {
	if s.dead.Load() {
		return errDead
	}
	return s.Store.Release(ctx, l)
}

func (s *deadStore) Rebuilds(ctx context.Context, name string) (uint64, uint64, error) {
	if s.dead.Load() {
		return 0, 0, errDead
	}
	return s.Store.Rebuilds(ctx, name)
}

func (s *deadStore) Heartbeat(ctx context.Context, name, owner string, version int, ttl time.Duration) ([]projection.Member, error) {
	if s.dead.Load() {
		return nil, errDead
	}
	return s.Store.Heartbeat(ctx, name, owner, version, ttl)
}

// TestRunner_CrashSpreadsShare covers an instance that crashes, keeping its
// claims and its member row until they expire: its segments spread over
// the rest instead of landing on one of them.
func TestRunner_CrashSpreadsShare(t *testing.T) {
	es, ps := &testStore{}, memory.NewProjectionStore()
	save(t, es, 5)
	defer runRunner(t, newSpreadRunner(es, ps, "a", 6, 0))()
	defer runRunner(t, newSpreadRunner(es, ps, "b", 6, 0))()
	cs := &deadStore{Store: ps}
	defer runRunner(t, newSpreadRunner(es, cs, "c", 6, 0))()
	waitSpread(t, ps, []string{"a", "b", "c"}, 6, 3*time.Second)

	cs.dead.Store(true)
	// c's claims and member row expire after the 1s ClaimTTL.
	waitHeld(t, ps, map[string]int{"a": 3, "b": 3}, 4*time.Second)
}

// TestRunner_RolloutLeavesNoSegmentFree covers old and new versions running
// together during a rolling deploy: the old instance counts for no share,
// so the new one takes every segment rather than leaving the old one's
// share unclaimed while it steps aside.
func TestRunner_RolloutLeavesNoSegmentFree(t *testing.T) {
	es, ps := &testStore{}, memory.NewProjectionStore()
	save(t, es, 5)
	defer runRunner(t, newSpreadRunner(es, ps, "old", 4, 1))()
	waitHeld(t, ps, map[string]int{"old": 4}, 2*time.Second)

	defer runRunner(t, newSpreadRunner(es, ps, "new", 4, 2))()
	waitHeld(t, ps, map[string]int{"new": 4}, 3*time.Second)
	// And it stays that way: the old instance claims nothing back.
	time.Sleep(200 * time.Millisecond)
	waitHeld(t, ps, map[string]int{"new": 4}, 0)
}
