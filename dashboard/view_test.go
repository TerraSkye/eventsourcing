package dashboard

import (
	"testing"
	"time"

	"github.com/terraskye/eventsourcing/projection"
)

func TestView_TakesTheWorstSegment(t *testing.T) {
	v := view("orders", []projection.Status{
		{Segment: 0, Phase: projection.Live, Position: 20, Head: 20, Owner: "a"},
		{Segment: 1, Phase: projection.CatchingUp, Position: 4, Head: 18, Owner: "b"},
	}, time.Minute)

	if v.State != stateCatchingUp || v.Position != 4 || v.Head != 20 || v.Behind != 14 {
		t.Errorf("view = state %s, position %d, head %d, behind %d; want catching_up, 4, 20, 14",
			v.State, v.Position, v.Head, v.Behind)
	}
}

func TestView_NamesASilentOwner(t *testing.T) {
	v := view("orders", []projection.Status{
		{Segment: 0, Phase: projection.Live, Position: 9, Head: 9, Owner: "a", Age: time.Minute},
	}, 15*time.Second)

	if v.State != stateSilent {
		t.Errorf("state = %s, want %s for an owner silent past the claim TTL", v.State, stateSilent)
	}
}

func TestView_NotStartedWithoutStatuses(t *testing.T) {
	if v := view("orders", nil, time.Minute); v.State != stateNotStarted {
		t.Errorf("state = %s, want %s", v.State, stateNotStarted)
	}
}

func TestRate_StartsOverAfterARebuild(t *testing.T) {
	var r rate
	at := time.Now()
	r.observe(0, at)
	r.observe(1000, at.Add(time.Second))
	if r.perSecond <= 0 {
		t.Fatalf("rate = %v after 1000 positions in a second, want it positive", r.perSecond)
	}
	r.observe(10, at.Add(2*time.Second)) // a rebuild moved the projection back
	if r.perSecond != 0 {
		t.Errorf("rate = %v after the position moved back, want 0", r.perSecond)
	}
}

// TestView_LiveIsNotBehind covers live segments that last read at
// different moments: the projection is not behind.
func TestView_LiveIsNotBehind(t *testing.T) {
	v := view("orders", []projection.Status{
		{Segment: 0, Phase: projection.Live, Position: 100, Head: 100, Owner: "a"},
		{Segment: 1, Phase: projection.Live, Position: 140, Head: 140, Owner: "a"},
	}, time.Minute)
	if v.State != stateLive || v.Behind != 0 {
		t.Errorf("view = state %s, behind %d; want live, 0", v.State, v.Behind)
	}
}

func TestView_ShowsAReplay(t *testing.T) {
	v := view("orders", []projection.Status{
		{Segment: 0, Phase: projection.CatchingUp, Position: 30, Head: 90, ReplayUntil: 80, Owner: "a"},
	}, time.Minute)
	if v.State != stateReplaying || v.Segments[0].Replay != 80 {
		t.Errorf("view = state %s, replay until %d; want replaying until 80", v.State, v.Segments[0].Replay)
	}
}

func TestView_ShowsAPendingRebuild(t *testing.T) {
	v := view("orders", []projection.Status{
		{Segment: 0, Phase: projection.Live, Position: 9, Head: 9, Owner: "a", RebuildRequested: true},
	}, time.Minute)
	if v.State != stateRebuildRequested || !v.RebuildRequested {
		t.Errorf("view = state %s, requested %v; want rebuild_requested", v.State, v.RebuildRequested)
	}

	stalled := view("orders", []projection.Status{
		{Segment: 0, Phase: projection.Stalled, Position: 9, Head: 12, Owner: "a", RebuildRequested: true},
	}, time.Minute)
	if stalled.State != stateStalled {
		t.Errorf("state = %s for a stalled segment with a pending rebuild, want stalled first", stalled.State)
	}
}

// TestSegmentState_UsesClaimTTL covers a status that carries its claim's
// ttl: a segment is silent by that ttl, not by the dashboard's own.
func TestSegmentState_UsesClaimTTL(t *testing.T) {
	st := projection.Status{Phase: projection.Live, Owner: "a", Age: 10 * time.Second}
	if got := segmentState(st, 5*time.Second); got != stateSilent {
		t.Errorf("without a ClaimTTL, state = %s, want %s by the dashboard's 5s", got, stateSilent)
	}
	st.ClaimTTL = 15 * time.Second
	if got := segmentState(st, 5*time.Second); got != stateLive {
		t.Errorf("with a 15s ClaimTTL, state = %s, want %s", got, stateLive)
	}
}

// TestInstancesView covers the instances the page lists, a standby one
// that holds no segment included.
func TestInstancesView(t *testing.T) {
	got := instancesView([]projection.Instance{
		{Owner: "a/1", Version: 2, Segments: 3, Age: 1500 * time.Millisecond},
		{Owner: "b/2", Version: 2, Segments: 0, Age: 0},
	})
	want := []instanceView{{Owner: "a/1", Version: 2, Segments: 3, AgeMs: 1500}, {Owner: "b/2", Version: 2}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("instancesView = %+v, want %+v", got, want)
	}
}
