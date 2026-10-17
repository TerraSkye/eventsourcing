// Package projectiontest provides [StoreAcceptanceTest], the contract every
// [projection.Store] implementation in this module is checked against. Each
// implementation, and any third-party store, runs the same suite from its
// own tests:
//
//	func TestAcceptance(t *testing.T) {
//		projectiontest.StoreAcceptanceTest(t, memory.NewProjectionStore())
//	}
package projectiontest

import (
	"context"
	"errors"
	"sort"
	"sync"
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

	mu     sync.Mutex
	leases map[leaseKey]projection.Lease // the last Lease Claim granted each owner
}

type leaseKey struct {
	name    string
	segment int
	owner   string
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
// If store implements [projection.DeadLetterStore], the suite also checks
// that dead letters saved and deleted inside a Commit load back per
// segment, ordered by position.
func StoreAcceptanceTest(t *testing.T, store projection.Store) {
	t.Helper()

	s := &storeAcceptance{store: store, leases: make(map[leaseKey]projection.Lease)}
	s.group(t, "Declare", map[string]func(*testing.T){
		"CreatesSegments":     s.declareCreatesSegments,
		"Idempotent":          s.declareIdempotent,
		"SegmentsChanged":     s.declareSegmentsChanged,
		"KeepsStoredProgress": s.declareKeepsStoredProgress,
		"SetsReplayUntil":     s.declareSetsReplayUntil,
	})
	s.group(t, "Claim", map[string]func(*testing.T){
		"Fresh":               s.claimFresh,
		"Refresh":             s.claimRefresh,
		"SameOwnerNewProcess": s.claimSameOwnerNewProcess,
		"TakeoverRaisesEpoch": s.claimTakeoverRaisesEpoch,
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
		"StaleEpoch":         s.commitStaleEpoch,
		"RefreshesHeartbeat": s.commitRefreshesHeartbeat,
	})
	s.group(t, "Reset", map[string]func(*testing.T){
		"MovesCheckpoint":     s.resetMovesCheckpoint,
		"RunsWipeOnce":        s.resetRunsWipeOnce,
		"WipeErrorKeepsState": s.resetWipeErrorKeepsState,
		"NotOwner":            s.resetNotOwner,
		"StaleEpoch":          s.resetStaleEpoch,
	})
	s.group(t, "Release", map[string]func(*testing.T){
		"LetsOtherClaim": s.releaseLetsOtherClaim,
		"ClearsOwner":    s.releaseClearsOwner,
		"MarksStandby":   s.releaseMarksStandby,
		"NotOwnerNoop":   s.releaseNotOwnerNoop,
		"StaleEpochNoop": s.releaseStaleEpochNoop,
	})
	s.group(t, "SaveStatus", map[string]func(*testing.T){
		"Publishes":       s.saveStatusPublishes,
		"KeepsCheckpoint": s.saveStatusKeepsCheckpoint,
		"NotOwnerRefused": s.saveStatusNotOwnerRefused,
		"StaleEpoch":      s.saveStatusStaleEpoch,
	})
	s.group(t, "LoadStatuses", map[string]func(*testing.T){
		"UnknownName": s.loadStatusesUnknownName,
		"Age":         s.loadStatusesAge,
	})
	if _, ok := store.(projection.DeadLetterStore); ok {
		s.group(t, "DeadLetter", map[string]func(*testing.T){
			"InsideCommit":        s.deadLetterInsideCommit,
			"LoadOrdered":         s.deadLetterLoadOrdered,
			"Delete":              s.deadLetterDelete,
			"SegmentsIndependent": s.deadLetterSegmentsIndependent,
			"UnknownName":         s.deadLetterUnknownName,
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

// --- Declare ---

func (s *storeAcceptance) declareCreatesSegments(t *testing.T) {
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

func (s *storeAcceptance) declareIdempotent(t *testing.T) {
	name := s.newProjection(t, 2)
	if err := s.store.Declare(t.Context(), name, 2, 0); err != nil {
		t.Fatalf("second Declare with the same segments: %v", err)
	}
	if n := len(s.statuses(t, name)); n != 2 {
		t.Fatalf("LoadStatuses returned %d statuses after a repeated Declare, want 2", n)
	}
}

func (s *storeAcceptance) declareSegmentsChanged(t *testing.T) {
	name := s.newProjection(t, 2)
	if err := s.store.Declare(t.Context(), name, 3, 0); !errors.Is(err, projection.ErrSegmentsChanged) {
		t.Fatalf("Declare with a different segment count: got %v, want ErrSegmentsChanged", err)
	}
}

func (s *storeAcceptance) declareKeepsStoredProgress(t *testing.T) {
	name := s.newProjection(t, 1)
	s.claim(t, name, 0, owner, longTTL)
	cp := projection.Checkpoint{Position: 4, Version: 1, ReplayUntil: 2}
	s.commit(t, name, 0, owner, cp)

	if err := s.store.Declare(t.Context(), name, 1, 0); err != nil {
		t.Fatalf("second Declare: %v", err)
	}
	if got := s.claim(t, name, 0, owner, longTTL); got != cp {
		t.Fatalf("checkpoint after a repeated Declare: got %+v, want %+v", got, cp)
	}
}

func (s *storeAcceptance) declareSetsReplayUntil(t *testing.T) {
	name := "projectiontest-" + uuid.NewString()
	if err := s.store.Declare(t.Context(), name, 2, 9); err != nil {
		t.Fatalf("Declare(2, replayUntil 9): %v", err)
	}
	if err := s.store.Declare(t.Context(), name, 2, 12); err != nil {
		t.Fatalf("second Declare(2, replayUntil 12): %v", err)
	}
	for seg := range 2 {
		want := projection.Checkpoint{ReplayUntil: 9}
		if got := s.claim(t, name, seg, owner, longTTL); got != want {
			t.Errorf("Claim of segment %d = %+v, want %+v from the first Declare", seg, got, want)
		}
	}
}

// --- Claim ---

func (s *storeAcceptance) claimFresh(t *testing.T) {
	name := s.newProjection(t, 1)
	if got := s.claim(t, name, 0, owner, longTTL); got != (projection.Checkpoint{}) {
		t.Fatalf("Claim on a fresh segment returned %+v, want the zero Checkpoint", got)
	}
}

func (s *storeAcceptance) claimRefresh(t *testing.T) {
	name := s.newProjection(t, 1)
	s.claim(t, name, 0, owner, longTTL)
	first := s.lease(name, 0, owner)
	s.claim(t, name, 0, owner, longTTL)
	if got := s.lease(name, 0, owner); got != first {
		t.Fatalf("Lease after refreshing the claim = %+v, want %+v unchanged", got, first)
	}
	if first.Epoch == 0 {
		t.Fatalf("Claim granted Epoch 0, want a non-zero epoch")
	}
}

// claimSameOwnerNewProcess covers a second process under the same Owner,
// such as a restart under a fixed pod name: it does not hold the claim
// the first process got, so it waits like any other instance.
func (s *storeAcceptance) claimSameOwnerNewProcess(t *testing.T) {
	name := s.newProjection(t, 1)
	s.claim(t, name, 0, owner, longTTL)
	_, _, ok, err := s.store.Claim(t.Context(), newProcess(name), longTTL)
	if err != nil {
		t.Fatalf("Claim by a new process under the same Owner: %v", err)
	}
	if ok {
		t.Fatal("Claim by a new process under the same Owner was granted while the first holds the segment, want it refused")
	}
}

func (s *storeAcceptance) claimTakeoverRaisesEpoch(t *testing.T) {
	name := s.newProjection(t, 1)
	s.claim(t, name, 0, owner, longTTL)
	first := s.lease(name, 0, owner).Epoch
	s.release(t, name, 0, owner)
	s.claim(t, name, 0, other, shortTTL)
	second := s.lease(name, 0, other).Epoch
	time.Sleep(2 * shortTTL)
	s.claim(t, name, 0, owner, shortTTL)
	third := s.lease(name, 0, owner).Epoch
	if !(first < second && second < third) {
		t.Fatalf("epochs after a release and an expiry: %d, %d, %d; want each higher than the last", first, second, third)
	}
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
	cp := projection.Checkpoint{Position: 7, Version: 3, ReplayUntil: 5, Fingerprint: "fp-claim"}
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
	cp := projection.Checkpoint{Position: 10, Version: 2, ReplayUntil: 6, Fingerprint: "fp-commit"}
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
	if err := s.store.Commit(t.Context(), s.lease(name, 0, owner), cp, st, nop); err != nil {
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
	err := s.store.Commit(t.Context(), s.lease(name, 0, owner), projection.Checkpoint{Position: 1}, projection.Status{Phase: projection.Live, Position: 1, Owner: owner},
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

	err := s.store.Commit(t.Context(), s.lease(name, 0, owner),
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

// staleLease claims the segment as owner, lets the claim expire, and has a
// new process under the same Owner take it over. It returns the first
// claim's Lease, now stale, and the current one.
func (s *storeAcceptance) staleLease(t *testing.T, name string) (stale, current projection.Lease) {
	t.Helper()
	s.claim(t, name, 0, owner, shortTTL)
	stale = s.lease(name, 0, owner)
	time.Sleep(2 * shortTTL)
	current, _, ok, err := s.store.Claim(t.Context(), newProcess(name), shortTTL)
	if err != nil || !ok {
		t.Fatalf("Claim by a new process under the same Owner after expiry = %v, %v; want true, nil", ok, err)
	}
	s.setLease(current)
	return stale, current
}

func (s *storeAcceptance) commitStaleEpoch(t *testing.T) {
	name := s.newProjection(t, 1)
	stale, current := s.staleLease(t, name)

	cp := projection.Checkpoint{Position: 50, Version: 1}
	st := projection.Status{Phase: projection.Live, Position: 50, Owner: owner}
	if err := s.store.Commit(t.Context(), stale, cp, st, nop); !errors.Is(err, projection.ErrClaimLost) {
		t.Fatalf("Commit under a stale epoch: got %v, want ErrClaimLost", err)
	}
	if got := s.claim(t, name, 0, owner, longTTL); got != (projection.Checkpoint{}) {
		t.Fatalf("checkpoint after a stale-epoch Commit: got %+v, want it unchanged", got)
	}
	if err := s.store.Commit(t.Context(), current, cp, st, nop); err != nil {
		t.Fatalf("Commit under the current epoch: %v", err)
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
	s.commit(t, name, 0, owner, projection.Checkpoint{Position: 8, Version: 1, ReplayUntil: 2, Fingerprint: "fp-reset"})

	// The reset checkpoint describes nothing applied yet, so its
	// Fingerprint is empty until the next Commit.
	if err := s.store.Reset(t.Context(), s.lease(name, 0, owner), 2, nop); err != nil {
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
	err := s.store.Reset(t.Context(), s.lease(name, 0, owner), 1, func(context.Context) error {
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

	err := s.store.Reset(t.Context(), s.lease(name, 0, owner), 2, func(context.Context) error { return errWrite })
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

	err := s.store.Reset(t.Context(), s.lease(name, 0, other), 2, nop)
	if !errors.Is(err, projection.ErrClaimLost) {
		t.Fatalf("Reset by a non-owner: got %v, want ErrClaimLost", err)
	}
	if got := s.claim(t, name, 0, owner, longTTL); got != cp {
		t.Fatalf("checkpoint after a non-owner Reset: got %+v, want %+v", got, cp)
	}
}

func (s *storeAcceptance) resetStaleEpoch(t *testing.T) {
	name := s.newProjection(t, 1)
	stale, _ := s.staleLease(t, name)
	cp := projection.Checkpoint{Position: 5, Version: 1}
	s.commit(t, name, 0, owner, cp)

	err := s.store.Reset(t.Context(), stale, 2, nop)
	if !errors.Is(err, projection.ErrClaimLost) {
		t.Fatalf("Reset under a stale epoch: got %v, want ErrClaimLost", err)
	}
	if got := s.claim(t, name, 0, owner, longTTL); got != cp {
		t.Fatalf("checkpoint after a stale-epoch Reset: got %+v, want %+v", got, cp)
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

func (s *storeAcceptance) releaseMarksStandby(t *testing.T) {
	name := s.newProjection(t, 1)
	s.claim(t, name, 0, owner, longTTL)
	s.commit(t, name, 0, owner, projection.Checkpoint{Position: 1})
	s.release(t, name, 0, owner)
	if got := s.statuses(t, name)[0].Phase; got != projection.Standby {
		t.Fatalf("Phase after Release = %s, want %s", got, projection.Standby)
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

func (s *storeAcceptance) releaseStaleEpochNoop(t *testing.T) {
	name := s.newProjection(t, 1)
	stale, _ := s.staleLease(t, name)
	if err := s.store.Release(t.Context(), stale); err != nil {
		t.Fatalf("Release under a stale epoch: %v", err)
	}
	s.claimFails(t, name, 0, other, longTTL)
}

// --- SaveStatus ---

func (s *storeAcceptance) saveStatusPublishes(t *testing.T) {
	name := s.newProjection(t, 1)
	s.claim(t, name, 0, owner, longTTL)
	st := projection.Status{Phase: projection.Stalled, Head: 9, LastError: "boom", Owner: owner}
	if err := s.store.SaveStatus(t.Context(), s.lease(name, 0, owner), st); err != nil {
		t.Fatalf("SaveStatus: %v", err)
	}
	got := s.statuses(t, name)[0]
	if got.Phase != projection.Stalled || got.Head != 9 || got.LastError != "boom" || got.Owner != owner {
		t.Fatalf("status after SaveStatus: got %+v, want Phase stalled, Head 9, LastError \"boom\", Owner %q", got, owner)
	}

	st = projection.Status{Phase: projection.Live, Head: 9, Owner: owner}
	if err := s.store.SaveStatus(t.Context(), s.lease(name, 0, owner), st); err != nil {
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
	if err := s.store.SaveStatus(t.Context(), s.lease(name, 0, owner), st); err != nil {
		t.Fatalf("SaveStatus: %v", err)
	}
	if got := s.claim(t, name, 0, owner, longTTL); got != cp {
		t.Fatalf("checkpoint after SaveStatus: got %+v, want %+v", got, cp)
	}
	if got := s.statuses(t, name)[0]; got.Position != 6 || got.Version != 1 {
		t.Fatalf("status after SaveStatus: got Position %d, Version %d; want the checkpoint's 6, 1", got.Position, got.Version)
	}
}

func (s *storeAcceptance) saveStatusNotOwnerRefused(t *testing.T) {
	name := s.newProjection(t, 1)
	s.claim(t, name, 0, owner, longTTL)
	st := projection.Status{Phase: projection.Stalled, Head: 9, LastError: "boom", Owner: other}
	if err := s.store.SaveStatus(t.Context(), s.lease(name, 0, other), st); !errors.Is(err, projection.ErrClaimLost) {
		t.Errorf("SaveStatus by a non-owner = %v, want ErrClaimLost", err)
	}
	got := s.statuses(t, name)[0]
	if got.Phase == projection.Stalled || got.LastError != "" || got.Owner != owner {
		t.Fatalf("status after a non-owner SaveStatus: got %+v, want it unchanged", got)
	}
}

func (s *storeAcceptance) saveStatusStaleEpoch(t *testing.T) {
	name := s.newProjection(t, 1)
	stale, _ := s.staleLease(t, name)
	st := projection.Status{Phase: projection.Stalled, Head: 9, LastError: "boom", Owner: owner}
	if err := s.store.SaveStatus(t.Context(), stale, st); !errors.Is(err, projection.ErrClaimLost) {
		t.Errorf("SaveStatus under a stale epoch = %v, want ErrClaimLost", err)
	}
	if got := s.statuses(t, name)[0]; got.Phase == projection.Stalled || got.LastError != "" {
		t.Fatalf("status after a stale-epoch SaveStatus: got %+v, want it unchanged", got)
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
	name := s.newProjection(t, 1)
	s.claim(t, name, 0, owner, longTTL)
	want := projection.DeadLetter{Position: 1, StreamID: "order-1", EventType: "projectiontest.Event", Error: errWrite.Error()}
	s.saveDeadLetters(t, name, 0, want)
	if got := s.deadLetters(t, name, 0); len(got) != 1 || got[0] != want {
		t.Fatalf("LoadDeadLetters = %+v, want [%+v]", got, want)
	}
}

func (s *storeAcceptance) deadLetterLoadOrdered(t *testing.T) {
	name := s.newProjection(t, 1)
	s.claim(t, name, 0, owner, longTTL)
	s.saveDeadLetters(t, name, 0,
		projection.DeadLetter{Position: 7, StreamID: "order-2", EventType: "projectiontest.Event", Error: "b"},
		projection.DeadLetter{Position: 3, StreamID: "order-1", EventType: "projectiontest.Event", Error: "a"},
	)
	got := s.deadLetters(t, name, 0)
	if len(got) != 2 || got[0].Position != 3 || got[1].Position != 7 {
		t.Fatalf("LoadDeadLetters = %+v, want positions 3 then 7", got)
	}
}

func (s *storeAcceptance) deadLetterDelete(t *testing.T) {
	name := s.newProjection(t, 1)
	s.claim(t, name, 0, owner, longTTL)
	s.saveDeadLetters(t, name, 0,
		projection.DeadLetter{Position: 3, StreamID: "order-1", EventType: "projectiontest.Event", Error: "a"},
		projection.DeadLetter{Position: 4, StreamID: "order-1", EventType: "projectiontest.Event", Error: "b"},
	)
	dls := s.store.(projection.DeadLetterStore)
	err := s.store.Commit(t.Context(), s.lease(name, 0, owner), projection.Checkpoint{Position: 9}, projection.Status{Phase: projection.Live, Position: 9, Owner: owner},
		func(ctx context.Context) error {
			return dls.DeleteDeadLetter(ctx, s.lease(name, 0, owner), 3)
		})
	if err != nil {
		t.Fatalf("Commit with DeleteDeadLetter: %v", err)
	}
	if got := s.deadLetters(t, name, 0); len(got) != 1 || got[0].Position != 4 {
		t.Fatalf("LoadDeadLetters after deleting position 3 = %+v, want only position 4", got)
	}
}

func (s *storeAcceptance) deadLetterSegmentsIndependent(t *testing.T) {
	name := s.newProjection(t, 2)
	s.claim(t, name, 0, owner, longTTL)
	s.claim(t, name, 1, owner, longTTL)
	s.saveDeadLetters(t, name, 1, projection.DeadLetter{Position: 5, StreamID: "order-1", EventType: "projectiontest.Event", Error: "a"})
	if got := s.deadLetters(t, name, 0); len(got) != 0 {
		t.Fatalf("segment 0 holds %+v, want no dead letters", got)
	}
	if got := s.deadLetters(t, name, 1); len(got) != 1 {
		t.Fatalf("segment 1 holds %+v, want one dead letter", got)
	}
}

func (s *storeAcceptance) deadLetterUnknownName(t *testing.T) {
	got, err := s.store.(projection.DeadLetterStore).LoadDeadLetters(t.Context(), "projectiontest-"+uuid.NewString(), 0)
	if err != nil || len(got) != 0 {
		t.Fatalf("LoadDeadLetters for an unknown name = %+v, %v; want none, nil", got, err)
	}
}

// saveDeadLetters records ds for segment in one Commit by owner.
func (s *storeAcceptance) saveDeadLetters(t *testing.T, name string, segment int, ds ...projection.DeadLetter) {
	t.Helper()
	dls := s.store.(projection.DeadLetterStore)
	l := s.lease(name, segment, owner)
	err := s.store.Commit(t.Context(), l, projection.Checkpoint{Position: 1}, projection.Status{Phase: projection.Live, Position: 1, Owner: owner, Segment: segment},
		func(ctx context.Context) error {
			for _, d := range ds {
				if err := dls.SaveDeadLetter(ctx, l, d); err != nil {
					return err
				}
			}
			return nil
		})
	if err != nil {
		t.Fatalf("Commit with SaveDeadLetter: %v", err)
	}
}

func (s *storeAcceptance) deadLetters(t *testing.T, name string, segment int) []projection.DeadLetter {
	t.Helper()
	got, err := s.store.(projection.DeadLetterStore).LoadDeadLetters(t.Context(), name, segment)
	if err != nil {
		t.Fatalf("LoadDeadLetters: %v", err)
	}
	return got
}

// --- helpers ---

func nop(context.Context) error { return nil }

// newProjection initializes a projection with a fresh name and returns it.
func (s *storeAcceptance) newProjection(t *testing.T, segments int) string {
	t.Helper()
	name := "projectiontest-" + uuid.NewString()
	if err := s.store.Declare(t.Context(), name, segments, 0); err != nil {
		t.Fatalf("Declare(%d): %v", segments, err)
	}
	return name
}

// lease returns the last Lease Claim granted who on segment, or a Lease
// with Epoch 0 when who never claimed it.
func (s *storeAcceptance) lease(name string, segment int, who string) projection.Lease {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.leases[leaseKey{name, segment, who}]; ok {
		return l
	}
	return projection.Lease{Name: name, Segment: segment, Owner: who}
}

func (s *storeAcceptance) setLease(l projection.Lease) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leases[leaseKey{l.Name, l.Segment, l.Owner}] = l
}

// claim claims segment for who, under the Lease who last got, and fails
// the test if it is not granted.
func (s *storeAcceptance) claim(t *testing.T, name string, segment int, who string, ttl time.Duration) projection.Checkpoint {
	t.Helper()
	l, cp, ok, err := s.store.Claim(t.Context(), s.lease(name, segment, who), ttl)
	if err != nil {
		t.Fatalf("Claim(%d, %s): %v", segment, who, err)
	}
	if !ok {
		t.Fatalf("Claim(%d, %s) was refused, want it granted", segment, who)
	}
	if l.Name != name || l.Segment != segment || l.Owner != who {
		t.Fatalf("Claim(%d, %s) returned Lease %+v, want the same Name, Segment and Owner", segment, who, l)
	}
	s.setLease(l)
	return cp
}

// claimFails fails the test if segment can be claimed by who.
func (s *storeAcceptance) claimFails(t *testing.T, name string, segment int, who string, ttl time.Duration) {
	t.Helper()
	_, _, ok, err := s.store.Claim(t.Context(), s.lease(name, segment, who), ttl)
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
	if err := s.store.Commit(t.Context(), s.lease(name, segment, who), cp, st, nop); err != nil {
		t.Fatalf("Commit(%d, %s): %v", segment, who, err)
	}
}

// commitLost checks that a Commit by who returns ErrClaimLost.
func (s *storeAcceptance) commitLost(t *testing.T, name string, segment int, who string) {
	t.Helper()
	cp := projection.Checkpoint{Position: 50, Version: 1}
	st := projection.Status{Phase: projection.Live, Position: 50, Owner: who, Segment: segment}
	if err := s.store.Commit(t.Context(), s.lease(name, segment, who), cp, st, nop); !errors.Is(err, projection.ErrClaimLost) {
		t.Fatalf("Commit(%d, %s): got %v, want ErrClaimLost", segment, who, err)
	}
}

func (s *storeAcceptance) release(t *testing.T, name string, segment int, who string) {
	t.Helper()
	if err := s.store.Release(t.Context(), s.lease(name, segment, who)); err != nil {
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

// newProcess returns the Lease a new process under owner starts with: it
// has not claimed segment 0 of name yet, so its Epoch is 0.
func newProcess(name string) projection.Lease {
	return projection.Lease{Name: name, Segment: 0, Owner: owner}
}
