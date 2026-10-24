// Package projectiontest provides [StoreAcceptanceTest], the contract every
// [projection.Store] implementation in this module is checked against. Each
// implementation, and any third-party store, runs the same suite from its
// own tests:
//
//	func TestAcceptance(t *testing.T) {
//		projectiontest.StoreAcceptanceTest(t, memory.NewProjectionStore())
//	}
//
// [Benchmark] measures a projection runner on top of a store, so that
// numbers compare across backends.
package projectiontest

import (
	"context"
	"errors"
	"slices"
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

	// waitTimeout bounds a wait on a store call running in a goroutine, so
	// a store that breaks the contract fails the subtest instead of hanging.
	waitTimeout = 10 * time.Second
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
		"ExpiresByHolderTTL":  s.claimExpiresByHolderTTL,
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
		"KeepsReplayUntil":    s.resetKeepsReplayUntil,
		"RunsWipeOnce":        s.resetRunsWipeOnce,
		"WipeErrorKeepsState": s.resetWipeErrorKeepsState,
		"NotOwner":            s.resetNotOwner,
		"StaleEpoch":          s.resetStaleEpoch,
		"CoversEverySegment":  s.resetCoversEverySegment,
		"WaitsForCommit":      s.resetWaitsForCommit,
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
		"ReplayUntil": s.loadStatusesReplayUntil,
		"ClaimTTL":    s.loadStatusesClaimTTL,
	})
	s.group(t, "RebuildRequests", map[string]func(*testing.T){
		"UnknownName":   s.rebuildUnknownName,
		"Counted":       s.rebuildCounted,
		"ServedByReset": s.rebuildServedByReset,
		"InStatus":      s.rebuildInStatus,
	})
	s.group(t, "Heartbeat", map[string]func(*testing.T){
		"ListsSelf":        s.heartbeatListsSelf,
		"OrderedByOwner":   s.heartbeatOrderedByOwner,
		"Expires":          s.heartbeatExpires,
		"ExpiresByOwnTTL":  s.heartbeatExpiresByOwnTTL,
		"ZeroTTLRemoves":   s.heartbeatZeroTTLRemoves,
		"CountsSegments":   s.heartbeatCountsSegments,
		"UpdatesVersion":   s.heartbeatUpdatesVersion,
		"Age":              s.heartbeatAge,
		"NamesIndependent": s.heartbeatNamesIndependent,
	})
	s.group(t, "LoadInstances", map[string]func(*testing.T){
		"UnknownName":      s.loadInstancesUnknownName,
		"DoesNotRegister":  s.loadInstancesDoesNotRegister,
		"ExpiresByOwnTTL":  s.loadInstancesExpiresByOwnTTL,
		"MatchesHeartbeat": s.loadInstancesMatchesHeartbeat,
	})
	if _, ok := store.(projection.DeadLetterStore); ok {
		s.group(t, "DeadLetter", map[string]func(*testing.T){
			"InsideCommit":        s.deadLetterInsideCommit,
			"LoadOrdered":         s.deadLetterLoadOrdered,
			"Delete":              s.deadLetterDelete,
			"SegmentsIndependent": s.deadLetterSegmentsIndependent,
			"UnknownName":         s.deadLetterUnknownName,
			"ClearedByReset":      s.deadLetterClearedByReset,
		})
		s.group(t, "Replay", map[string]func(*testing.T){
			"UnknownName":       s.replayUnknownName,
			"NothingToReplay":   s.replayNothingToReplay,
			"PendingPerSegment": s.replayPendingPerSegment,
			"Finish":            s.replayFinish,
			"FinishFenced":      s.replayFinishFenced,
			"SucceededDeleted":  s.replaySucceededDeleted,
			"NumbersIncrease":   s.replayNumbersIncrease,
			"ClearedByReset":    s.replayClearedByReset,
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

// claimExpiresByHolderTTL covers a claim taken with a long ttl: a claimer
// with a shorter ttl still waits for the holder's, as instances configured
// with different ClaimTTLs must judge a claim alike.
func (s *storeAcceptance) claimExpiresByHolderTTL(t *testing.T) {
	name := s.newProjection(t, 1)
	s.claim(t, name, 0, owner, longTTL)
	time.Sleep(2 * shortTTL)
	s.claimFails(t, name, 0, other, shortTTL)
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

// resetKeepsReplayUntil covers a Reset before the segment got through its
// replay: the boundary stays where it was, as a rebuild can only add events
// to the replayed ones.
func (s *storeAcceptance) resetKeepsReplayUntil(t *testing.T) {
	name := s.newProjection(t, 1)
	s.claim(t, name, 0, owner, longTTL)
	s.commit(t, name, 0, owner, projection.Checkpoint{Position: 3, Version: 1, ReplayUntil: 9, Fingerprint: "fp-reset"})

	if err := s.store.Reset(t.Context(), s.lease(name, 0, owner), 2, nop); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	want := projection.Checkpoint{Position: 0, Version: 2, ReplayUntil: 9}
	if got := s.claim(t, name, 0, owner, longTTL); got != want {
		t.Fatalf("checkpoint after a Reset at 3 with ReplayUntil 9: got %+v, want %+v", got, want)
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

// resetCoversEverySegment covers that Reset rebuilds the whole projection:
// every segment's checkpoint moves back, the caller keeps its claim, and
// every other claim is released, so its holder can no longer commit into
// the wiped read model but can claim the segment again at once.
func (s *storeAcceptance) resetCoversEverySegment(t *testing.T) {
	name := s.newProjection(t, 3)
	s.claim(t, name, 0, owner, longTTL)
	s.claim(t, name, 1, other, longTTL)
	s.commit(t, name, 0, owner, projection.Checkpoint{Position: 8, Version: 1, Fingerprint: "fp-all"})
	s.commit(t, name, 1, other, projection.Checkpoint{Position: 6, Version: 1, Fingerprint: "fp-all"})

	if err := s.store.Reset(t.Context(), s.lease(name, 0, owner), 2, nop); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	for _, st := range s.statuses(t, name) {
		if st.Position != 0 || st.Version != 2 {
			t.Errorf("segment %d after Reset: Position %d, Version %d; want 0, 2", st.Segment, st.Position, st.Version)
		}
		if st.Segment != 0 && (st.Owner != "" || st.Phase != projection.Standby) {
			t.Errorf("segment %d after Reset: Owner %q, Phase %s; want released and standby", st.Segment, st.Owner, st.Phase)
		}
	}

	err := s.store.Commit(t.Context(), s.lease(name, 1, other), projection.Checkpoint{Position: 9, Version: 1}, projection.Status{}, nop)
	if !errors.Is(err, projection.ErrClaimLost) {
		t.Fatalf("Commit by the other segment's holder after Reset: got %v, want ErrClaimLost", err)
	}

	for seg, want := range map[int]projection.Checkpoint{
		1: {Version: 2, ReplayUntil: 6},
		2: {Version: 2},
	} {
		if got := s.claim(t, name, seg, other, longTTL); got != want {
			t.Errorf("segment %d claimed after Reset: got %+v, want %+v", seg, got, want)
		}
	}
	if got := s.claim(t, name, 0, owner, longTTL); got != (projection.Checkpoint{Version: 2, ReplayUntil: 8}) {
		t.Errorf("segment 0 after Reset: got %+v, want its caller to keep it at the new checkpoint", got)
	}
}

// resetWaitsForCommit covers that a Reset does not run while another
// segment's Commit is in progress, so no batch writes into the read model
// after wipe has emptied it.
func (s *storeAcceptance) resetWaitsForCommit(t *testing.T) {
	name := s.newProjection(t, 2)
	s.claim(t, name, 0, owner, longTTL)
	s.claim(t, name, 1, other, longTTL)

	entered, release := make(chan struct{}), make(chan struct{})
	committed := make(chan error, 1)
	go func() {
		committed <- s.store.Commit(t.Context(), s.lease(name, 1, other), projection.Checkpoint{Position: 4, Version: 1}, projection.Status{}, func(context.Context) error {
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case err := <-committed:
		t.Fatalf("Commit returned %v without running fn", err)
	case <-time.After(waitTimeout):
		t.Fatal("Commit did not run fn")
	}

	reset := make(chan error, 1)
	go func() { reset <- s.store.Reset(t.Context(), s.lease(name, 0, owner), 2, nop) }()
	select {
	case err := <-reset:
		close(release)
		t.Fatalf("Reset returned %v while another segment's Commit was in progress, want it to wait", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	for _, step := range []struct {
		name string
		done chan error
	}{{"Commit that started before the Reset", committed}, {"Reset", reset}} {
		select {
		case err := <-step.done:
			if err != nil {
				t.Fatalf("%s: %v", step.name, err)
			}
		case <-time.After(waitTimeout):
			t.Fatalf("%s did not return", step.name)
		}
	}
	if got := s.claim(t, name, 1, other, longTTL); got != (projection.Checkpoint{Version: 2, ReplayUntil: 4}) {
		t.Fatalf("segment 1 after the Reset: got %+v, want it reset after the Commit landed", got)
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

// --- Heartbeat ---

func (s *storeAcceptance) heartbeatListsSelf(t *testing.T) {
	name := s.newProjection(t, 1)
	got := s.heartbeat(t, name, owner, 2, longTTL)
	if len(got) != 1 || got[0].Owner != owner || got[0].Version != 2 || got[0].Segments != 0 {
		t.Fatalf("Heartbeat = %+v, want only %s on version 2 holding 0 segments", got, owner)
	}
}

func (s *storeAcceptance) heartbeatOrderedByOwner(t *testing.T) {
	name := s.newProjection(t, 1)
	s.heartbeat(t, name, other, 0, longTTL)
	s.heartbeat(t, name, "owner-c", 0, longTTL)
	got := s.heartbeat(t, name, owner, 0, longTTL)
	if want := []string{owner, other, "owner-c"}; !slices.Equal(instanceOwners(got), want) {
		t.Fatalf("Heartbeat returned owners %v, want %v", instanceOwners(got), want)
	}
}

func (s *storeAcceptance) heartbeatExpires(t *testing.T) {
	name := s.newProjection(t, 1)
	s.heartbeat(t, name, other, 0, shortTTL)
	time.Sleep(2 * shortTTL)
	if got := s.heartbeat(t, name, owner, 0, shortTTL); !slices.Equal(instanceOwners(got), []string{owner}) {
		t.Fatalf("Heartbeat returned owners %v after %s stopped for twice the ttl, want only %s", instanceOwners(got), other, owner)
	}
}

// heartbeatExpiresByOwnTTL covers instances that send heartbeats with
// different ttls: a caller with a short ttl still lists an instance whose
// longer ttl has not passed, and does not delete its row.
func (s *storeAcceptance) heartbeatExpiresByOwnTTL(t *testing.T) {
	name := s.newProjection(t, 1)
	s.heartbeat(t, name, other, 0, longTTL)
	time.Sleep(2 * shortTTL)
	s.heartbeat(t, name, owner, 0, shortTTL)
	if got := s.heartbeat(t, name, owner, 0, shortTTL); !slices.Equal(instanceOwners(got), []string{owner, other}) {
		t.Fatalf("Heartbeat with a short ttl returned owners %v, want %s, whose own ttl has not passed, as well", instanceOwners(got), other)
	}
}

func (s *storeAcceptance) heartbeatZeroTTLRemoves(t *testing.T) {
	name := s.newProjection(t, 1)
	s.heartbeat(t, name, other, 0, longTTL)
	s.heartbeat(t, name, other, 0, 0)
	if got := s.heartbeat(t, name, owner, 0, longTTL); !slices.Equal(instanceOwners(got), []string{owner}) {
		t.Fatalf("Heartbeat returned owners %v after %s left with ttl 0, want only %s", instanceOwners(got), other, owner)
	}
}

func (s *storeAcceptance) heartbeatCountsSegments(t *testing.T) {
	name := s.newProjection(t, 3)
	s.claim(t, name, 0, owner, longTTL)
	s.claim(t, name, 1, owner, longTTL)
	s.claim(t, name, 2, other, longTTL)
	s.release(t, name, 1, owner)
	s.heartbeat(t, name, other, 0, longTTL)
	got := s.heartbeat(t, name, owner, 0, longTTL)
	if len(got) != 2 || got[0].Segments != 1 || got[1].Segments != 1 {
		t.Fatalf("Heartbeat = %+v, want %s and %s holding 1 segment each", got, owner, other)
	}
}

func (s *storeAcceptance) heartbeatUpdatesVersion(t *testing.T) {
	name := s.newProjection(t, 1)
	s.heartbeat(t, name, owner, 1, longTTL)
	if got := s.heartbeat(t, name, owner, 2, longTTL); len(got) != 1 || got[0].Version != 2 {
		t.Fatalf("Heartbeat = %+v, want one instance on version 2", got)
	}
}

func (s *storeAcceptance) heartbeatAge(t *testing.T) {
	name := s.newProjection(t, 1)
	s.heartbeat(t, name, other, 0, longTTL)
	time.Sleep(shortTTL)
	got := s.heartbeat(t, name, owner, 0, longTTL)
	if len(got) != 2 {
		t.Fatalf("Heartbeat = %+v, want two instances", got)
	}
	if age := got[1].Age; age < shortTTL/2 || age > time.Minute {
		t.Fatalf("Age of %s is %s after its heartbeat %s ago, want roughly that", other, age, shortTTL)
	}
	if age := got[0].Age; age > shortTTL/2 {
		t.Fatalf("Age of %s is %s right after its heartbeat, want about 0", owner, age)
	}
}

func (s *storeAcceptance) heartbeatNamesIndependent(t *testing.T) {
	a, b := s.newProjection(t, 1), s.newProjection(t, 1)
	s.heartbeat(t, a, other, 0, longTTL)
	if got := s.heartbeat(t, b, owner, 0, longTTL); !slices.Equal(instanceOwners(got), []string{owner}) {
		t.Fatalf("Heartbeat on another projection returned owners %v, want only %s", instanceOwners(got), owner)
	}
}

func (s *storeAcceptance) loadStatusesClaimTTL(t *testing.T) {
	name := s.newProjection(t, 2)
	s.claim(t, name, 1, owner, longTTL)
	sts := s.statuses(t, name)
	if sts[0].ClaimTTL != 0 || sts[1].ClaimTTL != longTTL {
		t.Fatalf("ClaimTTL of an unowned segment and one claimed for %s = %s, %s; want 0, %s", longTTL, sts[0].ClaimTTL, sts[1].ClaimTTL, longTTL)
	}
}

// --- LoadInstances ---

func (s *storeAcceptance) loadInstancesUnknownName(t *testing.T) {
	if got := s.instances(t, "projectiontest-"+uuid.NewString()); len(got) != 0 {
		t.Fatalf("LoadInstances for an unknown name = %+v, want none", got)
	}
}

func (s *storeAcceptance) loadInstancesDoesNotRegister(t *testing.T) {
	name := s.newProjection(t, 1)
	s.heartbeat(t, name, other, 0, longTTL)
	for range 2 {
		if got := s.instances(t, name); !slices.Equal(instanceOwners(got), []string{other}) {
			t.Fatalf("LoadInstances returned owners %v, want only %s, which sent a heartbeat", instanceOwners(got), other)
		}
	}
}

func (s *storeAcceptance) loadInstancesExpiresByOwnTTL(t *testing.T) {
	name := s.newProjection(t, 1)
	s.heartbeat(t, name, owner, 0, longTTL)
	s.heartbeat(t, name, other, 0, shortTTL)
	time.Sleep(2 * shortTTL)
	if got := s.instances(t, name); !slices.Equal(instanceOwners(got), []string{owner}) {
		t.Fatalf("LoadInstances returned owners %v after %s's ttl passed, want only %s", instanceOwners(got), other, owner)
	}
}

func (s *storeAcceptance) loadInstancesMatchesHeartbeat(t *testing.T) {
	name := s.newProjection(t, 3)
	s.claim(t, name, 0, other, longTTL)
	s.claim(t, name, 1, other, longTTL)
	s.heartbeat(t, name, other, 2, longTTL)
	want := s.heartbeat(t, name, owner, 1, longTTL)
	got := s.instances(t, name)
	if len(got) != len(want) {
		t.Fatalf("LoadInstances = %+v, want the %d instances Heartbeat listed: %+v", got, len(want), want)
	}
	for i := range want {
		if got[i].Owner != want[i].Owner || got[i].Version != want[i].Version || got[i].Segments != want[i].Segments {
			t.Errorf("LoadInstances[%d] = %+v, want %+v as Heartbeat listed it", i, got[i], want[i])
		}
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
// deadLetterClearedByReset covers that Reset deletes the dead letters of
// every segment: the replay runs those events through the handler again,
// so letters left behind would park their streams for the whole replay and
// be recorded a second time.
func (s *storeAcceptance) deadLetterClearedByReset(t *testing.T) {
	name := s.newProjection(t, 2)
	s.claim(t, name, 0, owner, longTTL)
	s.claim(t, name, 1, owner, longTTL)
	s.saveDeadLetters(t, name, 0, projection.DeadLetter{Position: 1, StreamID: "order-1", EventType: "projectiontest.Event", Error: "a"})
	s.saveDeadLetters(t, name, 1, projection.DeadLetter{Position: 1, StreamID: "order-2", EventType: "projectiontest.Event", Error: "b"})

	if err := s.store.Reset(t.Context(), s.lease(name, 0, owner), 2, nop); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	for seg := range 2 {
		if got := s.deadLetters(t, name, seg); len(got) != 0 {
			t.Errorf("segment %d dead letters after Reset = %+v, want none", seg, got)
		}
	}
}

// --- Replay ---

func (s *storeAcceptance) replayUnknownName(t *testing.T) {
	_, err := s.store.(projection.DeadLetterStore).RequestReplay(t.Context(), "projectiontest-"+uuid.NewString(), "order-1")
	if !errors.Is(err, projection.ErrUnknownProjection) {
		t.Fatalf("RequestReplay for an unknown name: got %v, want ErrUnknownProjection", err)
	}
}

func (s *storeAcceptance) replayNothingToReplay(t *testing.T) {
	name := s.newProjection(t, 1)
	n := s.requestReplay(t, name, "order-1")
	if got := s.pendingReplays(t, name); len(got) != 0 {
		t.Errorf("PendingReplays for a stream without dead letters = %+v, want none", got)
	}
	if pending, errs := s.replayResult(t, name, n); pending != 0 || len(errs) != 0 {
		t.Errorf("ReplayResult = %d pending, errors %v; want nothing pending", pending, errs)
	}
}

// replayPendingPerSegment covers a stream with dead letters in two of
// three segments, next to another stream's: the request is pending for
// those two segments only.
func (s *storeAcceptance) replayPendingPerSegment(t *testing.T) {
	name := s.replayProjection(t)
	n := s.requestReplay(t, name, "order-1")
	want := []projection.Replay{{Number: n, StreamID: "order-1", Segment: 0}, {Number: n, StreamID: "order-1", Segment: 2}}
	if got := s.pendingReplays(t, name); !slices.Equal(got, want) {
		t.Fatalf("PendingReplays = %+v, want %+v", got, want)
	}
	if pending, _ := s.replayResult(t, name, n); pending != 2 {
		t.Fatalf("ReplayResult = %d pending, want 2", pending)
	}
}

func (s *storeAcceptance) replayFinish(t *testing.T) {
	name := s.replayProjection(t)
	n := s.requestReplay(t, name, "order-1")
	dls := s.store.(projection.DeadLetterStore)
	if err := dls.FinishReplay(t.Context(), s.lease(name, 0, owner), n, ""); err != nil {
		t.Fatalf("FinishReplay(0): %v", err)
	}
	if pending, errs := s.replayResult(t, name, n); pending != 1 || len(errs) != 0 {
		t.Fatalf("ReplayResult after segment 0 = %d pending, errors %v; want 1 pending and none", pending, errs)
	}
	if err := dls.FinishReplay(t.Context(), s.lease(name, 2, owner), n, "handler failed"); err != nil {
		t.Fatalf("FinishReplay(2): %v", err)
	}
	if pending, errs := s.replayResult(t, name, n); pending != 0 || !slices.Equal(errs, []string{"handler failed"}) {
		t.Fatalf("ReplayResult after both = %d pending, errors %v; want 0 and [handler failed]", pending, errs)
	}
	if got := s.pendingReplays(t, name); len(got) != 0 {
		t.Errorf("PendingReplays after both finished = %+v, want none", got)
	}
}

// replaySucceededDeleted covers a request every segment tried without an
// error: it is deleted, with the earlier finished request of its stream
// that failed, but not with the earlier one still pending.
func (s *storeAcceptance) replaySucceededDeleted(t *testing.T) {
	name := s.replayProjection(t)
	dls := s.store.(projection.DeadLetterStore)
	finish := func(n uint64, seg int, errMsg string) {
		t.Helper()
		if err := dls.FinishReplay(t.Context(), s.lease(name, seg, owner), n, errMsg); err != nil {
			t.Fatalf("FinishReplay(%d, segment %d): %v", n, seg, err)
		}
	}
	failed := s.requestReplay(t, name, "order-1")
	finish(failed, 0, "")
	finish(failed, 2, "handler failed")
	if pending, errs := s.replayResult(t, name, failed); pending != 0 || !slices.Equal(errs, []string{"handler failed"}) {
		t.Fatalf("ReplayResult of the failed request = %d pending, errors %v; want 0 and [handler failed]", pending, errs)
	}
	waiting := s.requestReplay(t, name, "order-1")
	succeeded := s.requestReplay(t, name, "order-1")
	finish(succeeded, 0, "")
	finish(succeeded, 2, "")

	if pending, errs := s.replayResult(t, name, failed); pending != 0 || len(errs) != 0 {
		t.Errorf("ReplayResult of the failed request after a later one succeeded = %d pending, errors %v; want it deleted", pending, errs)
	}
	if pending, errs := s.replayResult(t, name, succeeded); pending != 0 || len(errs) != 0 {
		t.Errorf("ReplayResult of the succeeded request = %d pending, errors %v; want nothing", pending, errs)
	}
	want := []projection.Replay{{Number: waiting, StreamID: "order-1", Segment: 0}, {Number: waiting, StreamID: "order-1", Segment: 2}}
	if got := s.pendingReplays(t, name); !slices.Equal(got, want) {
		t.Errorf("PendingReplays = %+v, want the request still pending %+v", got, want)
	}
}

func (s *storeAcceptance) replayFinishFenced(t *testing.T) {
	name := s.replayProjection(t)
	n := s.requestReplay(t, name, "order-1")
	err := s.store.(projection.DeadLetterStore).FinishReplay(t.Context(), s.lease(name, 0, other), n, "")
	if !errors.Is(err, projection.ErrClaimLost) {
		t.Fatalf("FinishReplay by an owner without the claim: got %v, want ErrClaimLost", err)
	}
	if pending, _ := s.replayResult(t, name, n); pending != 2 {
		t.Fatalf("ReplayResult after a refused FinishReplay = %d pending, want 2", pending)
	}
}

func (s *storeAcceptance) replayNumbersIncrease(t *testing.T) {
	name := s.replayProjection(t)
	first := s.requestReplay(t, name, "order-1")
	second := s.requestReplay(t, name, "order-1")
	if second <= first {
		t.Fatalf("second RequestReplay returned %d after %d, want a higher number", second, first)
	}
	if got := s.pendingReplays(t, name); len(got) != 4 {
		t.Fatalf("PendingReplays = %+v, want both requests on segments 0 and 2", got)
	}
}

func (s *storeAcceptance) replayClearedByReset(t *testing.T) {
	name := s.replayProjection(t)
	n := s.requestReplay(t, name, "order-1")
	if err := s.store.Reset(t.Context(), s.lease(name, 0, owner), 2, nop); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if got := s.pendingReplays(t, name); len(got) != 0 {
		t.Errorf("PendingReplays after Reset = %+v, want none", got)
	}
	if pending, errs := s.replayResult(t, name, n); pending != 0 || len(errs) != 0 {
		t.Errorf("ReplayResult after Reset = %d pending, errors %v; want nothing pending", pending, errs)
	}
	if err := s.store.(projection.DeadLetterStore).FinishReplay(t.Context(), s.lease(name, 0, owner), n, ""); err != nil {
		t.Errorf("FinishReplay of a request Reset deleted: %v, want nil", err)
	}
}

// replayProjection declares a projection of three segments, all claimed by
// owner, with dead letters of order-1 in segments 0 and 2 and of order-2
// in segment 1.
func (s *storeAcceptance) replayProjection(t *testing.T) string {
	t.Helper()
	name := s.newProjection(t, 3)
	for seg := range 3 {
		s.claim(t, name, seg, owner, longTTL)
	}
	letter := func(stream string) projection.DeadLetter {
		return projection.DeadLetter{Position: 1, StreamID: stream, EventType: "projectiontest.Event", Error: "boom"}
	}
	s.saveDeadLetters(t, name, 0, letter("order-1"))
	s.saveDeadLetters(t, name, 1, letter("order-2"))
	s.saveDeadLetters(t, name, 2, letter("order-1"))
	return name
}

func (s *storeAcceptance) requestReplay(t *testing.T, name, streamID string) uint64 {
	t.Helper()
	n, err := s.store.(projection.DeadLetterStore).RequestReplay(t.Context(), name, streamID)
	if err != nil {
		t.Fatalf("RequestReplay(%s): %v", streamID, err)
	}
	return n
}

func (s *storeAcceptance) pendingReplays(t *testing.T, name string) []projection.Replay {
	t.Helper()
	got, err := s.store.(projection.DeadLetterStore).PendingReplays(t.Context(), name)
	if err != nil {
		t.Fatalf("PendingReplays: %v", err)
	}
	return got
}

func (s *storeAcceptance) replayResult(t *testing.T, name string, n uint64) (int, []string) {
	t.Helper()
	pending, errs, err := s.store.(projection.DeadLetterStore).ReplayResult(t.Context(), name, n)
	if err != nil {
		t.Fatalf("ReplayResult(%d): %v", n, err)
	}
	return pending, errs
}

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

// --- LoadStatuses: ReplayUntil ---

func (s *storeAcceptance) loadStatusesReplayUntil(t *testing.T) {
	name := "projectiontest-" + uuid.NewString()
	if err := s.store.Declare(t.Context(), name, 2, 7); err != nil {
		t.Fatalf("Declare: %v", err)
	}
	for _, st := range s.statuses(t, name) {
		if st.ReplayUntil != 7 {
			t.Errorf("segment %d ReplayUntil = %d after Declare at 7, want 7", st.Segment, st.ReplayUntil)
		}
	}

	s.claim(t, name, 0, owner, longTTL)
	s.commit(t, name, 0, owner, projection.Checkpoint{Position: 9, Version: 1})
	if err := s.store.Reset(t.Context(), s.lease(name, 0, owner), 1, nop); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if st := s.statuses(t, name)[0]; st.ReplayUntil != 9 {
		t.Errorf("segment 0 ReplayUntil = %d after a Reset from 9, want 9", st.ReplayUntil)
	}
}

// --- RebuildRequests ---

func (s *storeAcceptance) rebuildUnknownName(t *testing.T) {
	name := "projectiontest-unknown-" + uuid.NewString()
	if _, err := s.store.RequestRebuild(t.Context(), name); !errors.Is(err, projection.ErrUnknownProjection) {
		t.Errorf("RequestRebuild of an undeclared projection: got %v, want ErrUnknownProjection", err)
	}
	requested, done, err := s.store.Rebuilds(t.Context(), name)
	if err != nil || requested != 0 || done != 0 {
		t.Errorf("Rebuilds of an undeclared projection = %d, %d, %v; want 0, 0, nil", requested, done, err)
	}
}

// rebuildCounted covers that requests are numbered per projection and
// kept until a Reset serves them.
func (s *storeAcceptance) rebuildCounted(t *testing.T) {
	name := s.newProjection(t, 2)
	for want := uint64(1); want <= 2; want++ {
		n, err := s.store.RequestRebuild(t.Context(), name)
		if err != nil || n != want {
			t.Fatalf("RequestRebuild = %d, %v; want %d, nil", n, err, want)
		}
	}
	if requested, done, err := s.store.Rebuilds(t.Context(), name); err != nil || requested != 2 || done != 0 {
		t.Fatalf("Rebuilds = %d, %d, %v; want 2, 0, nil", requested, done, err)
	}
	unrelated := s.newProjection(t, 1)
	if requested, _, _ := s.store.Rebuilds(t.Context(), unrelated); requested != 0 {
		t.Errorf("another projection's requests = %d, want 0: requests are per projection", requested)
	}
}

// rebuildServedByReset covers that a Reset marks every request made so far
// done, and a later request pending again.
func (s *storeAcceptance) rebuildServedByReset(t *testing.T) {
	name := s.newProjection(t, 2)
	s.requestRebuild(t, name)
	s.requestRebuild(t, name)

	s.claim(t, name, 0, owner, longTTL)
	if err := s.store.Reset(t.Context(), s.lease(name, 0, owner), 1, nop); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if requested, done, err := s.store.Rebuilds(t.Context(), name); err != nil || requested != 2 || done != 2 {
		t.Fatalf("Rebuilds after Reset = %d, %d, %v; want 2, 2, nil", requested, done, err)
	}

	s.requestRebuild(t, name)
	if requested, done, _ := s.store.Rebuilds(t.Context(), name); requested != 3 || done != 2 {
		t.Errorf("Rebuilds after a later request = %d, %d; want 3, 2", requested, done)
	}
}

// rebuildInStatus covers that LoadStatuses shows a pending rebuild on
// every segment until a Reset serves it.
func (s *storeAcceptance) rebuildInStatus(t *testing.T) {
	name := s.newProjection(t, 2)
	for _, st := range s.statuses(t, name) {
		if st.RebuildRequested {
			t.Fatalf("segment %d shows a rebuild requested before any was", st.Segment)
		}
	}
	s.requestRebuild(t, name)
	for _, st := range s.statuses(t, name) {
		if !st.RebuildRequested {
			t.Errorf("segment %d does not show the requested rebuild", st.Segment)
		}
	}
	s.claim(t, name, 0, owner, longTTL)
	if err := s.store.Reset(t.Context(), s.lease(name, 0, owner), 1, nop); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	for _, st := range s.statuses(t, name) {
		if st.RebuildRequested {
			t.Errorf("segment %d still shows a rebuild requested after Reset", st.Segment)
		}
	}
}

func (s *storeAcceptance) requestRebuild(t *testing.T, name string) {
	t.Helper()
	if _, err := s.store.RequestRebuild(t.Context(), name); err != nil {
		t.Fatalf("RequestRebuild: %v", err)
	}
}

// newProjection initializes a projection with a fresh name and returns it.
func (s *storeAcceptance) newProjection(t *testing.T, segments int) string {
	t.Helper()
	name := "projectiontest-" + uuid.NewString()
	if err := s.store.Declare(t.Context(), name, segments, 0); err != nil {
		t.Fatalf("Declare(%d): %v", segments, err)
	}
	return name
}

// heartbeat calls Heartbeat and fails the test on an error.
func (s *storeAcceptance) heartbeat(t *testing.T, name, who string, version int, ttl time.Duration) []projection.Instance {
	t.Helper()
	instances, err := s.store.Heartbeat(t.Context(), name, who, version, ttl)
	if err != nil {
		t.Fatalf("Heartbeat(%s, ttl %s): %v", who, ttl, err)
	}
	return instances
}

// instances calls LoadInstances and fails the test on an error.
func (s *storeAcceptance) instances(t *testing.T, name string) []projection.Instance {
	t.Helper()
	instances, err := s.store.LoadInstances(t.Context(), name)
	if err != nil {
		t.Fatalf("LoadInstances: %v", err)
	}
	return instances
}

// instanceOwners returns the owners of instances, in order.
func instanceOwners(instances []projection.Instance) []string {
	owners := make([]string, len(instances))
	for i, m := range instances {
		owners[i] = m.Owner
	}
	return owners
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
