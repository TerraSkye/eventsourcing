// Package projectiontest provides [StoreAcceptanceTest], the contract every
// [projection.Store] implementation in this module is checked against. Each
// implementation, and any third-party store, runs the same suite from its
// own tests:
//
//	func TestAcceptance(t *testing.T) {
//		projectiontest.StoreAcceptanceTest(t, memory.NewStore())
//	}
package projectiontest

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing/projection"
)

const (
	owner = "owner-a"
	other = "owner-b"

	// longTTL keeps a claim valid for the whole subtest.
	longTTL = time.Hour

	// shortTTL lets a subtest wait a claim out. Stores may track heartbeats
	// at a coarse resolution, so expiry is checked after twice this long.
	shortTTL = 300 * time.Millisecond
)

var errWrite = errors.New("projectiontest: fn failed")

type storeAcceptance struct {
	store projection.Store
}

// StoreAcceptanceTest runs the [projection.Store] contract against store as
// a set of subtests.
//
// store may already hold projections, and may be shared with other tests:
// every subtest uses a projection of its own, named with a fresh UUID. A
// few subtests wait for a claim to expire, so the suite takes a few
// seconds.
//
// The suite cannot see the read model, so it does not check that a failed
// Commit or Reset rolls back the writes fn made through the store's unit of
// work; only that the checkpoint and status stay put. Implementations that
// open a real transaction should test that themselves.
//
// If store implements [projection.DeadLetterStore], SaveDeadLetter is
// called inside a Commit and must succeed.
func StoreAcceptanceTest(t *testing.T, store projection.Store) {
	t.Helper()

	s := &storeAcceptance{store: store}
	s.group(t, "Init", map[string]func(*testing.T){
		"CreatesSegments":     s.initCreatesSegments,
		"Idempotent":          s.initIdempotent,
		"SegmentsChanged":     s.initSegmentsChanged,
		"KeepsStoredProgress": s.initKeepsStoredProgress,
	})
	s.group(t, "Claim", map[string]func(*testing.T){
		"Fresh":               s.claimFresh,
		"SameOwner":           s.claimSameOwner,
		"HeldByOther":         s.claimHeldByOther,
		"Expired":             s.claimExpired,
		"SegmentsIndependent": s.claimSegmentsIndependent,
		"ReturnsCheckpoint":   s.claimReturnsCheckpoint,
		"SetsOwner":           s.claimSetsOwner,
	})
	s.group(t, "Commit", map[string]func(*testing.T){
		"StoresCheckpoint":   s.commitStoresCheckpoint,
		"StoresStatus":       s.commitStoresStatus,
		"RunsFnOnce":         s.commitRunsFnOnce,
		"FnErrorKeepsState":  s.commitFnErrorKeepsState,
		"NotOwner":           s.commitNotOwner,
		"AfterRelease":       s.commitAfterRelease,
		"AfterTakeover":      s.commitAfterTakeover,
		"RefreshesHeartbeat": s.commitRefreshesHeartbeat,
	})
	s.group(t, "Reset", map[string]func(*testing.T){
		"MovesCheckpoint":     s.resetMovesCheckpoint,
		"RunsWipeOnce":        s.resetRunsWipeOnce,
		"WipeErrorKeepsState": s.resetWipeErrorKeepsState,
		"NotOwner":            s.resetNotOwner,
	})
	s.group(t, "Release", map[string]func(*testing.T){
		"LetsOtherClaim": s.releaseLetsOtherClaim,
		"ClearsOwner":    s.releaseClearsOwner,
		"NotOwnerNoop":   s.releaseNotOwnerNoop,
	})
	s.group(t, "SaveStatus", map[string]func(*testing.T){
		"Publishes":       s.saveStatusPublishes,
		"KeepsCheckpoint": s.saveStatusKeepsCheckpoint,
		"NotOwnerIgnored": s.saveStatusNotOwnerIgnored,
	})
	s.group(t, "LoadStatuses", map[string]func(*testing.T){
		"UnknownName": s.loadStatusesUnknownName,
		"Age":         s.loadStatusesAge,
	})
	if _, ok := store.(projection.DeadLetterStore); ok {
		s.group(t, "DeadLetter", map[string]func(*testing.T){
			"InsideCommit": s.deadLetterInsideCommit,
		})
	}
}

// group runs tests as subtests of a subtest named name, in a stable order.
func (s *storeAcceptance) group(t *testing.T, name string, tests map[string]func(*testing.T)) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		names := make([]string, 0, len(tests))
		for n := range tests {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			t.Run(n, tests[n])
		}
	})
}

// --- Init ---

func (s *storeAcceptance) initCreatesSegments(t *testing.T) {
	name := s.newProjection(t, 3)
	sts := s.statuses(t, name)
	if len(sts) != 3 {
		t.Fatalf("LoadStatuses returned %d statuses, want 3", len(sts))
	}
	for i, st := range sts {
		if st.Segment != i {
			t.Errorf("status %d has Segment %d, want %d (ordered by segment)", i, st.Segment, i)
		}
		if st.Phase != projection.Starting || st.Position != 0 || st.Owner != "" {
			t.Errorf("segment %d: got Phase %s, Position %d, Owner %q; want starting, 0, \"\"", i, st.Phase, st.Position, st.Owner)
		}
	}
}

func (s *storeAcceptance) initIdempotent(t *testing.T) {
	name := s.newProjection(t, 2)
	if err := s.store.Init(t.Context(), name, 2); err != nil {
		t.Fatalf("second Init with the same segments: %v", err)
	}
	if n := len(s.statuses(t, name)); n != 2 {
		t.Fatalf("LoadStatuses returned %d statuses after a repeated Init, want 2", n)
	}
}

func (s *storeAcceptance) initSegmentsChanged(t *testing.T) {
	name := s.newProjection(t, 2)
	if err := s.store.Init(t.Context(), name, 3); !errors.Is(err, projection.ErrSegmentsChanged) {
		t.Fatalf("Init with a different segment count: got %v, want ErrSegmentsChanged", err)
	}
}

func (s *storeAcceptance) initKeepsStoredProgress(t *testing.T) {
	name := s.newProjection(t, 1)
	s.claim(t, name, 0, owner, longTTL)
	cp := projection.Checkpoint{Position: 4, Version: 1, ReplayUntil: 2}
	s.commit(t, name, 0, owner, cp)

	if err := s.store.Init(t.Context(), name, 1); err != nil {
		t.Fatalf("second Init: %v", err)
	}
	if got := s.claim(t, name, 0, owner, longTTL); got != cp {
		t.Fatalf("checkpoint after a repeated Init: got %+v, want %+v", got, cp)
	}
}

// --- Claim ---

func (s *storeAcceptance) claimFresh(t *testing.T) {
	name := s.newProjection(t, 1)
	if got := s.claim(t, name, 0, owner, longTTL); got != (projection.Checkpoint{}) {
		t.Fatalf("Claim on a fresh segment returned %+v, want the zero Checkpoint", got)
	}
}

func (s *storeAcceptance) claimSameOwner(t *testing.T) {
	name := s.newProjection(t, 1)
	s.claim(t, name, 0, owner, longTTL)
	s.claim(t, name, 0, owner, longTTL)
}

func (s *storeAcceptance) claimHeldByOther(t *testing.T) {
	name := s.newProjection(t, 1)
	s.claim(t, name, 0, owner, longTTL)
	s.claimFails(t, name, 0, other, longTTL)
}

func (s *storeAcceptance) claimExpired(t *testing.T) {
	name := s.newProjection(t, 1)
	s.claim(t, name, 0, owner, shortTTL)
	time.Sleep(2 * shortTTL)
	s.claim(t, name, 0, other, shortTTL)
}

func (s *storeAcceptance) claimSegmentsIndependent(t *testing.T) {
	name := s.newProjection(t, 2)
	s.claim(t, name, 0, owner, longTTL)
	s.claim(t, name, 1, other, longTTL)
}

func (s *storeAcceptance) claimReturnsCheckpoint(t *testing.T) {
	name := s.newProjection(t, 1)
	s.claim(t, name, 0, owner, longTTL)
	cp := projection.Checkpoint{Position: 7, Version: 3, ReplayUntil: 5}
	s.commit(t, name, 0, owner, cp)
	s.release(t, name, 0, owner)

	if got := s.claim(t, name, 0, other, longTTL); got != cp {
		t.Fatalf("Claim returned %+v, want the committed %+v", got, cp)
	}
}

func (s *storeAcceptance) claimSetsOwner(t *testing.T) {
	name := s.newProjection(t, 2)
	s.claim(t, name, 1, owner, longTTL)
	sts := s.statuses(t, name)
	if sts[0].Owner != "" || sts[1].Owner != owner {
		t.Fatalf("owners after claiming segment 1: got %q, %q; want \"\", %q", sts[0].Owner, sts[1].Owner, owner)
	}
}

// --- Commit ---

func (s *storeAcceptance) commitStoresCheckpoint(t *testing.T) {
	name := s.newProjection(t, 1)
	s.claim(t, name, 0, owner, longTTL)
	cp := projection.Checkpoint{Position: 10, Version: 2, ReplayUntil: 6}
	s.commit(t, name, 0, owner, cp)

	if got := s.claim(t, name, 0, owner, longTTL); got != cp {
		t.Fatalf("checkpoint after Commit: got %+v, want %+v", got, cp)
	}
}

func (s *storeAcceptance) commitStoresStatus(t *testing.T) {
	name := s.newProjection(t, 1)
	s.claim(t, name, 0, owner, longTTL)
	cp := projection.Checkpoint{Position: 10, Version: 2}
	st := projection.Status{Phase: projection.Live, Position: 10, Head: 12, Version: 2, Owner: owner}
	if err := s.store.Commit(t.Context(), projection.Lease{Name: name, Segment: 0, Owner: owner}, cp, st, nop); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	got := s.statuses(t, name)[0]
	if got.Phase != projection.Live || got.Position != 10 || got.Head != 12 || got.Version != 2 || got.Owner != owner || got.LastError != "" {
		t.Fatalf("status after Commit: got %+v, want Phase live, Position 10, Head 12, Version 2, Owner %q", got, owner)
	}
}

func (s *storeAcceptance) commitRunsFnOnce(t *testing.T) {
	name := s.newProjection(t, 1)
	s.claim(t, name, 0, owner, longTTL)
	calls := 0
	err := s.store.Commit(t.Context(), projection.Lease{Name: name, Segment: 0, Owner: owner}, projection.Checkpoint{Position: 1}, projection.Status{Phase: projection.Live, Position: 1, Owner: owner},
		func(ctx context.Context) error {
			calls++
			if ctx == nil {
				t.Error("fn got a nil ctx")
			}
			return nil
		})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if calls != 1 {
		t.Fatalf("fn called %d times, want 1", calls)
	}
}

func (s *storeAcceptance) commitFnErrorKeepsState(t *testing.T) {
	name := s.newProjection(t, 1)
	s.claim(t, name, 0, owner, longTTL)
	cp := projection.Checkpoint{Position: 3, Version: 1}
	s.commit(t, name, 0, owner, cp)

	err := s.store.Commit(t.Context(), projection.Lease{Name: name, Segment: 0, Owner: owner},
		projection.Checkpoint{Position: 9, Version: 1},
		projection.Status{Phase: projection.Live, Position: 9, Owner: owner},
		func(context.Context) error { return errWrite })
	if !errors.Is(err, errWrite) {
		t.Fatalf("Commit with a failing fn: got %v, want fn's error", err)
	}
	if got := s.claim(t, name, 0, owner, longTTL); got != cp {
		t.Fatalf("checkpoint after a failed Commit: got %+v, want %+v", got, cp)
	}
	if got := s.statuses(t, name)[0].Position; got != 3 {
		t.Fatalf("status Position after a failed Commit: got %d, want 3", got)
	}
}

func (s *storeAcceptance) commitNotOwner(t *testing.T) {
	name := s.newProjection(t, 1)
	s.claim(t, name, 0, owner, longTTL)
	s.commitLost(t, name, 0, other)
	if got := s.claim(t, name, 0, owner, longTTL); got != (projection.Checkpoint{}) {
		t.Fatalf("checkpoint after a non-owner Commit: got %+v, want it unchanged", got)
	}
}

func (s *storeAcceptance) commitAfterRelease(t *testing.T) {
	name := s.newProjection(t, 1)
	s.claim(t, name, 0, owner, longTTL)
	s.release(t, name, 0, owner)
	s.commitLost(t, name, 0, owner)
}

func (s *storeAcceptance) commitAfterTakeover(t *testing.T) {
	name := s.newProjection(t, 1)
	s.claim(t, name, 0, owner, shortTTL)
	time.Sleep(2 * shortTTL)
	s.claim(t, name, 0, other, shortTTL)

	s.commitLost(t, name, 0, owner)
	if got := s.claim(t, name, 0, other, longTTL); got != (projection.Checkpoint{}) {
		t.Fatalf("new owner's checkpoint after the old owner's Commit: got %+v, want it unchanged", got)
	}
}

func (s *storeAcceptance) commitRefreshesHeartbeat(t *testing.T) {
	name := s.newProjection(t, 1)
	ttl := 4 * shortTTL
	s.claim(t, name, 0, owner, ttl)
	time.Sleep(3 * shortTTL)
	s.commit(t, name, 0, owner, projection.Checkpoint{Position: 1})
	time.Sleep(3 * shortTTL)

	// 6 shortTTL after the claim, but only 3 after the Commit.
	s.claimFails(t, name, 0, other, ttl)
}

// --- Reset ---

func (s *storeAcceptance) resetMovesCheckpoint(t *testing.T) {
	name := s.newProjection(t, 1)
	s.claim(t, name, 0, owner, longTTL)
	s.commit(t, name, 0, owner, projection.Checkpoint{Position: 8, Version: 1, ReplayUntil: 2})

	if err := s.store.Reset(t.Context(), projection.Lease{Name: name, Segment: 0, Owner: owner}, 2, nop); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	want := projection.Checkpoint{Position: 0, Version: 2, ReplayUntil: 8}
	if got := s.claim(t, name, 0, owner, longTTL); got != want {
		t.Fatalf("checkpoint after Reset: got %+v, want %+v", got, want)
	}
	if got := s.statuses(t, name)[0]; got.Position != 0 || got.Version != 2 {
		t.Fatalf("status after Reset: got Position %d, Version %d; want 0, 2", got.Position, got.Version)
	}
}

func (s *storeAcceptance) resetRunsWipeOnce(t *testing.T) {
	name := s.newProjection(t, 1)
	s.claim(t, name, 0, owner, longTTL)
	calls := 0
	err := s.store.Reset(t.Context(), projection.Lease{Name: name, Segment: 0, Owner: owner}, 1, func(context.Context) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if calls != 1 {
		t.Fatalf("wipe called %d times, want 1", calls)
	}
}

func (s *storeAcceptance) resetWipeErrorKeepsState(t *testing.T) {
	name := s.newProjection(t, 1)
	s.claim(t, name, 0, owner, longTTL)
	cp := projection.Checkpoint{Position: 5, Version: 1}
	s.commit(t, name, 0, owner, cp)

	err := s.store.Reset(t.Context(), projection.Lease{Name: name, Segment: 0, Owner: owner}, 2, func(context.Context) error { return errWrite })
	if !errors.Is(err, errWrite) {
		t.Fatalf("Reset with a failing wipe: got %v, want wipe's error", err)
	}
	if got := s.claim(t, name, 0, owner, longTTL); got != cp {
		t.Fatalf("checkpoint after a failed Reset: got %+v, want %+v", got, cp)
	}
}

func (s *storeAcceptance) resetNotOwner(t *testing.T) {
	name := s.newProjection(t, 1)
	s.claim(t, name, 0, owner, longTTL)
	cp := projection.Checkpoint{Position: 5, Version: 1}
	s.commit(t, name, 0, owner, cp)

	err := s.store.Reset(t.Context(), projection.Lease{Name: name, Segment: 0, Owner: other}, 2, nop)
	if !errors.Is(err, projection.ErrClaimLost) {
		t.Fatalf("Reset by a non-owner: got %v, want ErrClaimLost", err)
	}
	if got := s.claim(t, name, 0, owner, longTTL); got != cp {
		t.Fatalf("checkpoint after a non-owner Reset: got %+v, want %+v", got, cp)
	}
}

// --- Release ---

func (s *storeAcceptance) releaseLetsOtherClaim(t *testing.T) {
	name := s.newProjection(t, 1)
	s.claim(t, name, 0, owner, longTTL)
	s.release(t, name, 0, owner)
	s.claim(t, name, 0, other, longTTL)
}

func (s *storeAcceptance) releaseClearsOwner(t *testing.T) {
	name := s.newProjection(t, 1)
	s.claim(t, name, 0, owner, longTTL)
	s.release(t, name, 0, owner)
	if got := s.statuses(t, name)[0].Owner; got != "" {
		t.Fatalf("Owner after Release: got %q, want \"\"", got)
	}
}

func (s *storeAcceptance) releaseNotOwnerNoop(t *testing.T) {
	name := s.newProjection(t, 1)
	s.claim(t, name, 0, owner, longTTL)
	s.release(t, name, 0, other)
	s.claimFails(t, name, 0, other, longTTL)
	if got := s.statuses(t, name)[0].Owner; got != owner {
		t.Fatalf("Owner after a non-owner Release: got %q, want %q", got, owner)
	}
}

// --- SaveStatus ---

func (s *storeAcceptance) saveStatusPublishes(t *testing.T) {
	name := s.newProjection(t, 1)
	s.claim(t, name, 0, owner, longTTL)
	st := projection.Status{Phase: projection.Stalled, Head: 9, LastError: "boom", Owner: owner}
	if err := s.store.SaveStatus(t.Context(), projection.Lease{Name: name, Segment: 0, Owner: owner}, st); err != nil {
		t.Fatalf("SaveStatus: %v", err)
	}
	got := s.statuses(t, name)[0]
	if got.Phase != projection.Stalled || got.Head != 9 || got.LastError != "boom" || got.Owner != owner {
		t.Fatalf("status after SaveStatus: got %+v, want Phase stalled, Head 9, LastError \"boom\", Owner %q", got, owner)
	}

	st = projection.Status{Phase: projection.Live, Head: 9, Owner: owner}
	if err := s.store.SaveStatus(t.Context(), projection.Lease{Name: name, Segment: 0, Owner: owner}, st); err != nil {
		t.Fatalf("SaveStatus: %v", err)
	}
	if got := s.statuses(t, name)[0]; got.Phase != projection.Live || got.LastError != "" {
		t.Fatalf("status after clearing the error: got Phase %s, LastError %q; want live, \"\"", got.Phase, got.LastError)
	}
}

func (s *storeAcceptance) saveStatusKeepsCheckpoint(t *testing.T) {
	name := s.newProjection(t, 1)
	s.claim(t, name, 0, owner, longTTL)
	cp := projection.Checkpoint{Position: 6, Version: 1, ReplayUntil: 4}
	s.commit(t, name, 0, owner, cp)

	// A status whose Position and Version disagree with the checkpoint:
	// SaveStatus publishes phase, head and error, never progress.
	st := projection.Status{Phase: projection.Live, Position: 99, Version: 99, Head: 7, Owner: owner}
	if err := s.store.SaveStatus(t.Context(), projection.Lease{Name: name, Segment: 0, Owner: owner}, st); err != nil {
		t.Fatalf("SaveStatus: %v", err)
	}
	if got := s.claim(t, name, 0, owner, longTTL); got != cp {
		t.Fatalf("checkpoint after SaveStatus: got %+v, want %+v", got, cp)
	}
	if got := s.statuses(t, name)[0]; got.Position != 6 || got.Version != 1 {
		t.Fatalf("status after SaveStatus: got Position %d, Version %d; want the checkpoint's 6, 1", got.Position, got.Version)
	}
}

func (s *storeAcceptance) saveStatusNotOwnerIgnored(t *testing.T) {
	name := s.newProjection(t, 1)
	s.claim(t, name, 0, owner, longTTL)
	st := projection.Status{Phase: projection.Stalled, Head: 9, LastError: "boom", Owner: other}
	_ = s.store.SaveStatus(t.Context(), projection.Lease{Name: name, Segment: 0, Owner: other}, st) // may fail or be ignored
	got := s.statuses(t, name)[0]
	if got.Phase == projection.Stalled || got.LastError != "" || got.Owner != owner {
		t.Fatalf("status after a non-owner SaveStatus: got %+v, want it unchanged", got)
	}
}

// --- LoadStatuses ---

func (s *storeAcceptance) loadStatusesUnknownName(t *testing.T) {
	sts, err := s.store.LoadStatuses(t.Context(), "projectiontest-"+uuid.NewString())
	if err != nil {
		t.Fatalf("LoadStatuses for an unknown name: %v", err)
	}
	if len(sts) != 0 {
		t.Fatalf("LoadStatuses for an unknown name returned %d statuses, want 0", len(sts))
	}
}

func (s *storeAcceptance) loadStatusesAge(t *testing.T) {
	name := s.newProjection(t, 1)
	s.claim(t, name, 0, owner, longTTL)
	s.commit(t, name, 0, owner, projection.Checkpoint{Position: 1})
	time.Sleep(shortTTL)

	age := s.statuses(t, name)[0].Age
	if age < shortTTL/2 || age > time.Minute {
		t.Fatalf("Age %s after publishing %s ago, want roughly that", age, shortTTL)
	}
}

// --- DeadLetter ---

func (s *storeAcceptance) deadLetterInsideCommit(t *testing.T) {
	dls := s.store.(projection.DeadLetterStore)
	name := s.newProjection(t, 1)
	s.claim(t, name, 0, owner, longTTL)
	err := s.store.Commit(t.Context(), projection.Lease{Name: name, Segment: 0, Owner: owner}, projection.Checkpoint{Position: 1}, projection.Status{Phase: projection.Live, Position: 1, Owner: owner},
		func(ctx context.Context) error {
			return dls.SaveDeadLetter(ctx, projection.Lease{Name: name, Segment: 0, Owner: owner}, 1, "projectiontest.Event", errWrite)
		})
	if err != nil {
		t.Fatalf("Commit with SaveDeadLetter: %v", err)
	}
}

// --- helpers ---

func nop(context.Context) error { return nil }

// newProjection initializes a projection with a fresh name and returns it.
func (s *storeAcceptance) newProjection(t *testing.T, segments int) string {
	t.Helper()
	name := "projectiontest-" + uuid.NewString()
	if err := s.store.Init(t.Context(), name, segments); err != nil {
		t.Fatalf("Init(%d): %v", segments, err)
	}
	return name
}

// claim claims segment for who and fails the test if it is not granted.
func (s *storeAcceptance) claim(t *testing.T, name string, segment int, who string, ttl time.Duration) projection.Checkpoint {
	t.Helper()
	cp, ok, err := s.store.Claim(t.Context(), projection.Lease{Name: name, Segment: segment, Owner: who}, ttl)
	if err != nil {
		t.Fatalf("Claim(%d, %s): %v", segment, who, err)
	}
	if !ok {
		t.Fatalf("Claim(%d, %s) was refused, want it granted", segment, who)
	}
	return cp
}

// claimFails fails the test if segment can be claimed by who.
func (s *storeAcceptance) claimFails(t *testing.T, name string, segment int, who string, ttl time.Duration) {
	t.Helper()
	_, ok, err := s.store.Claim(t.Context(), projection.Lease{Name: name, Segment: segment, Owner: who}, ttl)
	if err != nil {
		t.Fatalf("Claim(%d, %s): %v", segment, who, err)
	}
	if ok {
		t.Fatalf("Claim(%d, %s) was granted, want it refused", segment, who)
	}
}

// commit commits cp with a matching Live status and no writes.
func (s *storeAcceptance) commit(t *testing.T, name string, segment int, who string, cp projection.Checkpoint) {
	t.Helper()
	st := projection.Status{Phase: projection.Live, Position: cp.Position, Head: cp.Position, Version: cp.Version, Owner: who, Segment: segment}
	if err := s.store.Commit(t.Context(), projection.Lease{Name: name, Segment: segment, Owner: who}, cp, st, nop); err != nil {
		t.Fatalf("Commit(%d, %s): %v", segment, who, err)
	}
}

// commitLost checks that a Commit by who returns ErrClaimLost.
func (s *storeAcceptance) commitLost(t *testing.T, name string, segment int, who string) {
	t.Helper()
	cp := projection.Checkpoint{Position: 50, Version: 1}
	st := projection.Status{Phase: projection.Live, Position: 50, Owner: who, Segment: segment}
	if err := s.store.Commit(t.Context(), projection.Lease{Name: name, Segment: segment, Owner: who}, cp, st, nop); !errors.Is(err, projection.ErrClaimLost) {
		t.Fatalf("Commit(%d, %s): got %v, want ErrClaimLost", segment, who, err)
	}
}

func (s *storeAcceptance) release(t *testing.T, name string, segment int, who string) {
	t.Helper()
	if err := s.store.Release(t.Context(), projection.Lease{Name: name, Segment: segment, Owner: who}); err != nil {
		t.Fatalf("Release(%d, %s): %v", segment, who, err)
	}
}

func (s *storeAcceptance) statuses(t *testing.T, name string) []projection.Status {
	t.Helper()
	sts, err := s.store.LoadStatuses(t.Context(), name)
	if err != nil {
		t.Fatalf("LoadStatuses: %v", err)
	}
	return sts
}
