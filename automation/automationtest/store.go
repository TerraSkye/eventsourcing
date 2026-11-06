package automationtest

import (
	"context"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing/automation"
)

const (
	// longTTL keeps a segment lease valid for the whole subtest.
	longTTL = time.Hour

	// turnGap separates two calls whose order the fairness rule depends
	// on. Stores may keep time at millisecond resolution, so two calls in
	// the same millisecond would tie.
	turnGap = 5 * time.Millisecond

	// pauseSlack is how far below the requested pause DeferredFor may
	// report: the time between the pause being set and being read.
	pauseSlack = 2 * time.Second
)

// StoreAcceptanceTest runs the [automation.Store] contract against store as
// a set of subtests: grants and their limits, fencing on owner and epoch,
// lease expiry, pauses, the fairness order across groups and keys, and the
// automation-wide pause and published statuses a dashboard reads.
//
// store may already hold automations and may be shared with other tests:
// every subtest uses an automation of its own, named with a fresh UUID. A
// few subtests wait for a lease to expire, so the suite takes a few
// seconds.
func StoreAcceptanceTest(t *testing.T, store automation.Store) {
	t.Helper()
	s := storeAcceptance{store: store}
	group(t, "Acquire", map[string]func(*testing.T){
		"GrantsUpToMax":             s.acquireGrantsUpToMax,
		"NeverGrantsAHeldKey":       s.acquireNeverGrantsAHeldKey,
		"MaxZeroRecordsAndCounts":   s.acquireMaxZero,
		"IgnoresDuplicateKeys":      s.acquireIgnoresDuplicates,
		"NoCandidates":              s.acquireNoCandidates,
		"ExpiredLeaseRaisesEpoch":   s.acquireExpiredRaisesEpoch,
		"NamesAreIndependent":       s.acquireNamesIndependent,
		"ConcurrentNeverDoubled":    s.acquireConcurrent,
		"RoundRobinsGroupsThenKeys": s.acquireRoundRobins,
		"KeepsGroupWhenOmitted":     s.acquireKeepsGroup,
	})
	group(t, "Renew", map[string]func(*testing.T){
		"ExtendsTheLease": s.renewExtends,
		"ReportsLost":     s.renewReportsLost,
	})
	group(t, "Release", map[string]func(*testing.T){
		"FreesTheKey":           s.releaseFrees,
		"FencedOnOwnerAndEpoch": s.releaseFenced,
		"DeferForPauses":        s.releaseDeferFor,
		"ServedGoesToTheBack":   s.releaseServed,
	})
	group(t, "Status", map[string]func(*testing.T){
		"PauseIsPerAutomation": s.pauseIsPerAutomation,
		"SavedStatuses":        s.statusesSaved,
		"ExpiredStatusLeftOut": s.statusesDropExpired,
		"Segments":             s.segmentsLoaded,
		"UnknownAutomation":    s.statusUnknown,
	})
	group(t, "Defer", map[string]func(*testing.T){
		"PausesGrants":        s.deferPauses,
		"KeepsTheLongerPause": s.deferKeepsLonger,
		"UnknownKey":          s.deferUnknownKey,
		"Unsegmented":         s.deferUnsegmented,
	})
}

type storeAcceptance struct {
	store automation.Store
}

// group runs tests as subtests of a subtest named name, in a stable order.
func group(t *testing.T, name string, tests map[string]func(*testing.T)) {
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

func newName() string { return "automationtest-" + uuid.NewString() }

func segs(keys ...string) []automation.Segment {
	out := make([]automation.Segment, len(keys))
	for i, k := range keys {
		out[i] = automation.Segment{Key: k}
	}
	return out
}

func keysOf(ls []automation.SegmentLease) string {
	ks := make([]string, len(ls))
	for i, l := range ls {
		ks[i] = l.Key
	}
	return strings.Join(ks, ",")
}

func (s storeAcceptance) acquire(t *testing.T, name, owner string, cands []automation.Segment, max int, ttl time.Duration) automation.AcquireResult {
	t.Helper()
	res, err := s.store.Acquire(t.Context(), name, owner, cands, max, ttl)
	if err != nil {
		t.Fatalf("Acquire(%s, max %d): %v", owner, max, err)
	}
	for _, l := range res.Granted {
		if l.Name != name || l.Owner != owner {
			t.Fatalf("Acquire granted %+v, want name %q and owner %q", l, name, owner)
		}
	}
	return res
}

func (s storeAcceptance) release(t *testing.T, rel ...automation.ReleaseRequest) {
	t.Helper()
	lost, err := s.store.Release(t.Context(), rel)
	if err != nil || len(lost) != 0 {
		t.Fatalf("Release = lost %v, %v; want none", lost, err)
	}
}

func (s storeAcceptance) deferredFor(t *testing.T, name, key string) time.Duration {
	t.Helper()
	d, err := s.store.DeferredFor(t.Context(), name, key)
	if err != nil {
		t.Fatalf("DeferredFor(%q): %v", key, err)
	}
	return d
}

// wantPaused fails unless DeferredFor reports a pause of about want.
func (s storeAcceptance) wantPaused(t *testing.T, name, key string, want time.Duration) {
	t.Helper()
	if d := s.deferredFor(t, name, key); d > want || d < want-pauseSlack {
		t.Fatalf("DeferredFor(%q) = %s, want about %s", key, d, want)
	}
}

// --- Acquire ---

func (s storeAcceptance) acquireGrantsUpToMax(t *testing.T) {
	name := newName()
	res := s.acquire(t, name, "o1", segs("a", "b", "c"), 2, longTTL)
	if len(res.Granted) != 2 || res.Active != 3 || res.Eligible != 1 {
		t.Fatalf("Acquire = %+v, want 2 granted, 3 active, 1 eligible", res)
	}
	if res.Granted[0].Key == res.Granted[1].Key {
		t.Fatalf("granted %q twice", res.Granted[0].Key)
	}
	for _, l := range res.Granted {
		if l.Epoch == 0 {
			t.Fatalf("grant %+v has epoch 0, want a raised epoch", l)
		}
	}
}

func (s storeAcceptance) acquireNeverGrantsAHeldKey(t *testing.T) {
	name := newName()
	cands := segs("a", "b")
	r1 := s.acquire(t, name, "o1", cands, 1, longTTL)
	r2 := s.acquire(t, name, "o2", cands, 2, longTTL)
	if len(r1.Granted) != 1 || len(r2.Granted) != 1 || r1.Granted[0].Key == r2.Granted[0].Key {
		t.Fatalf("o1 got %q, o2 got %q; want one different key each", keysOf(r1.Granted), keysOf(r2.Granted))
	}
	r3 := s.acquire(t, name, "o3", cands, 2, longTTL)
	if len(r3.Granted) != 0 || r3.Eligible != 0 || r3.Active != 2 {
		t.Fatalf("o3 = %+v, want nothing granted, nothing eligible, 2 active", r3)
	}
	// The holder itself is not granted its own key again either.
	if r := s.acquire(t, name, "o1", cands, 2, longTTL); len(r.Granted) != 0 {
		t.Fatalf("o1 was granted %q again", keysOf(r.Granted))
	}
}

func (s storeAcceptance) acquireMaxZero(t *testing.T) {
	name := newName()
	res := s.acquire(t, name, "o1", segs("a", "b"), 0, longTTL)
	if len(res.Granted) != 0 || res.Active != 2 || res.Eligible != 2 {
		t.Fatalf("Acquire(max 0) = %+v, want nothing granted, 2 active, 2 eligible", res)
	}
}

func (s storeAcceptance) acquireIgnoresDuplicates(t *testing.T) {
	name := newName()
	res := s.acquire(t, name, "o1", segs("a", "a", "b"), 3, longTTL)
	if len(res.Granted) != 2 || res.Active != 2 {
		t.Fatalf("Acquire = %+v, want a and b granted once each, 2 active", res)
	}
}

func (s storeAcceptance) acquireNoCandidates(t *testing.T) {
	res := s.acquire(t, newName(), "o1", nil, 3, longTTL)
	if len(res.Granted) != 0 || res.Active != 0 || res.Eligible != 0 {
		t.Fatalf("Acquire(no candidates) = %+v, want zero", res)
	}
}

func (s storeAcceptance) acquireExpiredRaisesEpoch(t *testing.T) {
	name := newName()
	const ttl = 300 * time.Millisecond
	r1 := s.acquire(t, name, "o1", segs("a"), 1, ttl)
	if len(r1.Granted) != 1 {
		t.Fatalf("o1 got %q, want a", keysOf(r1.Granted))
	}
	time.Sleep(2 * ttl)
	r2 := s.acquire(t, name, "o2", segs("a"), 1, ttl)
	if keysOf(r2.Granted) != "a" || r2.Granted[0].Epoch <= r1.Granted[0].Epoch {
		t.Fatalf("after expiry o2 got %+v, want a with an epoch above %d", r2.Granted, r1.Granted[0].Epoch)
	}
}

func (s storeAcceptance) acquireNamesIndependent(t *testing.T) {
	n1, n2 := newName(), newName()
	if r := s.acquire(t, n1, "o1", segs("a"), 1, longTTL); keysOf(r.Granted) != "a" {
		t.Fatalf("%s: got %q, want a", n1, keysOf(r.Granted))
	}
	if r := s.acquire(t, n2, "o2", segs("a"), 1, longTTL); keysOf(r.Granted) != "a" {
		t.Fatalf("%s: got %q, want a: a key held in one automation must not block another", n2, keysOf(r.Granted))
	}
	if err := s.store.Defer(t.Context(), n1, "b", time.Hour); err != nil {
		t.Fatal(err)
	}
	if d := s.deferredFor(t, n2, "b"); d != 0 {
		t.Fatalf("a pause in %s shows in %s as %s", n1, n2, d)
	}
}

func (s storeAcceptance) acquireConcurrent(t *testing.T) {
	name := newName()
	var keys []string
	for i := range 20 {
		keys = append(keys, string(rune('a'+i)))
	}
	cands := segs(keys...)

	const owners = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		granted = map[string]string{}
	)
	for i := range owners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			owner := "o" + string(rune('0'+i))
			for range 3 {
				res, err := s.store.Acquire(context.WithoutCancel(t.Context()), name, owner, cands, 2, longTTL)
				if err != nil {
					t.Errorf("Acquire(%s): %v", owner, err)
					return
				}
				mu.Lock()
				for _, l := range res.Granted {
					if prev, ok := granted[l.Key]; ok {
						t.Errorf("key %q granted to %s while %s holds it", l.Key, owner, prev)
					}
					granted[l.Key] = owner
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(granted) != len(keys) {
		t.Fatalf("%d keys granted, want all %d", len(granted), len(keys))
	}
}

// acquireRoundRobins checks the fairness order: groups take turns by the
// last time one of their keys was served, keys within a group by how long
// each has waited, and a multi-grant interleaves groups.
func (s storeAcceptance) acquireRoundRobins(t *testing.T) {
	name := newName()
	cands := []automation.Segment{
		{Key: "acme:stripe", Group: "acme"},
		{Key: "acme:shopify", Group: "acme"},
	}
	s.acquire(t, name, "o1", cands, 0, longTTL)
	time.Sleep(turnGap)
	cands = append(cands, automation.Segment{Key: "zed:stripe", Group: "zed"})

	res := s.acquire(t, name, "o1", cands, 3, longTTL)
	if got, want := keysOf(res.Granted), "acme:shopify,zed:stripe,acme:stripe"; got != want {
		t.Fatalf("multi-grant = %q, want %q", got, want)
	}
	var back []automation.ReleaseRequest
	for _, l := range res.Granted {
		back = append(back, automation.ReleaseRequest{Lease: l}) // not served: waits unchanged
	}
	s.release(t, back...)

	var served []string
	for range 6 {
		res := s.acquire(t, name, "o1", cands, 1, longTTL)
		if len(res.Granted) != 1 {
			t.Fatalf("grant %d: %q, want one key", len(served), keysOf(res.Granted))
		}
		served = append(served, res.Granted[0].Key)
		time.Sleep(turnGap)
		s.release(t, automation.ReleaseRequest{Lease: res.Granted[0], Served: true})
		time.Sleep(turnGap)
	}
	want := "acme:shopify,zed:stripe,acme:stripe,zed:stripe,acme:shopify,zed:stripe"
	if got := strings.Join(served, ","); got != want {
		t.Fatalf("served\n %s\nwant\n %s", got, want)
	}
}

// acquireKeepsGroup checks that a candidate without a group does not
// clear the group a key was recorded with.
func (s storeAcceptance) acquireKeepsGroup(t *testing.T) {
	name := newName()
	s.acquire(t, name, "o1", []automation.Segment{
		{Key: "a1", Group: "a"}, {Key: "a2", Group: "a"}, {Key: "b1", Group: "b"},
	}, 0, longTTL)
	// Without groups a1 and a2 would be granted first by key order; with
	// a's group kept, b1 comes between them.
	res := s.acquire(t, name, "o1", segs("a1", "a2", "b1"), 3, longTTL)
	if got, want := keysOf(res.Granted), "a1,b1,a2"; got != want {
		t.Fatalf("grant = %q, want %q", got, want)
	}
}

// --- Renew ---

func (s storeAcceptance) renewExtends(t *testing.T) {
	name := newName()
	const ttl = 400 * time.Millisecond
	res := s.acquire(t, name, "o1", segs("a"), 1, ttl)
	for range 3 {
		time.Sleep(ttl / 2)
		lost, err := s.store.Renew(t.Context(), res.Granted, ttl)
		if err != nil || len(lost) != 0 {
			t.Fatalf("Renew = lost %v, %v; want none", lost, err)
		}
	}
	// Renewed past its first ttl, the key is still held.
	if r := s.acquire(t, name, "o2", segs("a"), 1, ttl); len(r.Granted) != 0 {
		t.Fatalf("o2 was granted %q, a renewed key", keysOf(r.Granted))
	}
}

func (s storeAcceptance) renewReportsLost(t *testing.T) {
	name := newName()
	const ttl = 300 * time.Millisecond
	r1 := s.acquire(t, name, "o1", segs("a", "b"), 2, ttl)
	held := map[string]automation.SegmentLease{}
	for _, l := range r1.Granted {
		held[l.Key] = l
	}
	wrongOwner := held["b"]
	wrongOwner.Owner = "o9"
	unknown := automation.SegmentLease{Name: name, Key: "nope", Owner: "o1", Epoch: 1}

	lost, err := s.store.Renew(t.Context(), []automation.SegmentLease{held["a"], wrongOwner, unknown}, ttl)
	if err != nil {
		t.Fatal(err)
	}
	if got := sortedKeys(lost); got != "b,nope" {
		t.Fatalf("Renew lost %q, want b,nope", got)
	}

	// An expired lease is lost, even if nobody took the key yet.
	time.Sleep(2 * ttl)
	lost, err = s.store.Renew(t.Context(), []automation.SegmentLease{held["a"]}, ttl)
	if err != nil || keysOf(lost) != "a" {
		t.Fatalf("Renew of an expired lease lost %q, %v; want a", keysOf(lost), err)
	}

	// So is a lease whose key was granted again since.
	r2 := s.acquire(t, name, "o2", segs("b"), 1, longTTL)
	if keysOf(r2.Granted) != "b" {
		t.Fatalf("o2 got %q, want b", keysOf(r2.Granted))
	}
	lost, err = s.store.Renew(t.Context(), []automation.SegmentLease{held["b"]}, ttl)
	if err != nil || keysOf(lost) != "b" {
		t.Fatalf("Renew of a superseded lease lost %q, %v; want b", keysOf(lost), err)
	}
}

func sortedKeys(ls []automation.SegmentLease) string {
	ks := make([]string, len(ls))
	for i, l := range ls {
		ks[i] = l.Key
	}
	sort.Strings(ks)
	return strings.Join(ks, ",")
}

// --- Release ---

func (s storeAcceptance) releaseFrees(t *testing.T) {
	name := newName()
	res := s.acquire(t, name, "o1", segs("a"), 1, longTTL)
	s.release(t, automation.ReleaseRequest{Lease: res.Granted[0]})
	if d := s.deferredFor(t, name, "a"); d != 0 {
		t.Fatalf("released without DeferFor, paused for %s", d)
	}
	r2 := s.acquire(t, name, "o2", segs("a"), 1, longTTL)
	if keysOf(r2.Granted) != "a" || r2.Granted[0].Epoch <= res.Granted[0].Epoch {
		t.Fatalf("after release o2 got %+v, want a with an epoch above %d", r2.Granted, res.Granted[0].Epoch)
	}
	// The released lease is not renewable any more.
	lost, err := s.store.Renew(t.Context(), res.Granted, longTTL)
	if err != nil || keysOf(lost) != "a" {
		t.Fatalf("Renew after release lost %q, %v; want a", keysOf(lost), err)
	}
}

func (s storeAcceptance) releaseFenced(t *testing.T) {
	name := newName()
	const ttl = 300 * time.Millisecond
	r1 := s.acquire(t, name, "o1", segs("a"), 1, ttl)
	old := r1.Granted[0]
	time.Sleep(2 * ttl)
	r2 := s.acquire(t, name, "o2", segs("a"), 1, longTTL)
	if keysOf(r2.Granted) != "a" {
		t.Fatalf("o2 got %q, want a", keysOf(r2.Granted))
	}

	wrongOwner := r2.Granted[0]
	wrongOwner.Owner = "o1"
	lost, err := s.store.Release(t.Context(), []automation.ReleaseRequest{
		{Lease: old, DeferFor: time.Hour, Served: true},
		{Lease: wrongOwner, DeferFor: time.Hour},
	})
	if err != nil || len(lost) != 2 {
		t.Fatalf("stale Release lost %v, %v; want both", lost, err)
	}
	if d := s.deferredFor(t, name, "a"); d != 0 {
		t.Fatalf("a stale release paused the key for %s", d)
	}
	if r := s.acquire(t, name, "o3", segs("a"), 1, longTTL); len(r.Granted) != 0 {
		t.Fatal("a stale release freed the key for another owner")
	}
	if lost, err := s.store.Renew(t.Context(), r2.Granted, longTTL); err != nil || len(lost) != 0 {
		t.Fatalf("holder's Renew after a stale release lost %v, %v", lost, err)
	}
}

func (s storeAcceptance) releaseDeferFor(t *testing.T) {
	name := newName()
	res := s.acquire(t, name, "o1", segs("a", "b"), 2, longTTL)
	byKey := map[string]automation.SegmentLease{}
	for _, l := range res.Granted {
		byKey[l.Key] = l
	}
	s.release(t,
		automation.ReleaseRequest{Lease: byKey["a"], DeferFor: time.Minute},
		automation.ReleaseRequest{Lease: byKey["b"]},
	)
	s.wantPaused(t, name, "a", time.Minute)
	r := s.acquire(t, name, "o2", segs("a", "b"), 2, longTTL)
	if keysOf(r.Granted) != "b" || r.Active != 1 {
		t.Fatalf("grant = %q active %d, want only b and 1 active", keysOf(r.Granted), r.Active)
	}
}

func (s storeAcceptance) releaseServed(t *testing.T) {
	name := newName()
	cands := segs("a", "b")
	s.acquire(t, name, "o1", cands, 0, longTTL)
	time.Sleep(turnGap)

	// a is first by key order; served, it goes behind b.
	res := s.acquire(t, name, "o1", cands, 1, longTTL)
	if keysOf(res.Granted) != "a" {
		t.Fatalf("first grant %q, want a", keysOf(res.Granted))
	}
	time.Sleep(turnGap)
	s.release(t, automation.ReleaseRequest{Lease: res.Granted[0], Served: true})
	time.Sleep(turnGap)
	res = s.acquire(t, name, "o1", cands, 2, longTTL)
	if got := keysOf(res.Granted); got != "b,a" {
		t.Fatalf("after a was served grant = %q, want b,a", got)
	}

	// Released without Served, a key keeps its place.
	s.release(t, automation.ReleaseRequest{Lease: res.Granted[0]}, automation.ReleaseRequest{Lease: res.Granted[1]})
	time.Sleep(turnGap)
	res = s.acquire(t, name, "o1", cands, 2, longTTL)
	if got := keysOf(res.Granted); got != "b,a" {
		t.Fatalf("after an unserved release grant = %q, want b,a", got)
	}
}

// --- Defer ---

func (s storeAcceptance) deferPauses(t *testing.T) {
	name := newName()
	const pause = 400 * time.Millisecond
	if err := s.store.Defer(t.Context(), name, "a", pause); err != nil {
		t.Fatal(err)
	}
	res := s.acquire(t, name, "o1", segs("a", "b"), 2, longTTL)
	if keysOf(res.Granted) != "b" || res.Active != 1 || res.Eligible != 0 {
		t.Fatalf("grant = %+v, want only b, 1 active, 0 eligible", res)
	}
	time.Sleep(pause + 100*time.Millisecond)
	if d := s.deferredFor(t, name, "a"); d != 0 {
		t.Fatalf("pause passed, DeferredFor = %s, want 0", d)
	}
	if r := s.acquire(t, name, "o1", segs("a", "b"), 2, longTTL); keysOf(r.Granted) != "a" {
		t.Fatalf("after the pause grant = %q, want a", keysOf(r.Granted))
	}
}

func (s storeAcceptance) deferKeepsLonger(t *testing.T) {
	name := newName()
	ctx := t.Context()
	if err := s.store.Defer(ctx, name, "a", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.store.Defer(ctx, name, "a", time.Second); err != nil {
		t.Fatal(err)
	}
	s.wantPaused(t, name, "a", time.Minute)
	if err := s.store.Defer(ctx, name, "a", time.Hour); err != nil {
		t.Fatal(err)
	}
	s.wantPaused(t, name, "a", time.Hour)

	// A held key can be paused without disturbing its holder.
	res := s.acquire(t, name, "o1", segs("b"), 1, longTTL)
	if err := s.store.Defer(ctx, name, "b", time.Minute); err != nil {
		t.Fatal(err)
	}
	if lost, err := s.store.Renew(ctx, res.Granted, longTTL); err != nil || len(lost) != 0 {
		t.Fatalf("Renew after Defer lost %v, %v; want none", lost, err)
	}
	s.wantPaused(t, name, "b", time.Minute)
}

func (s storeAcceptance) deferUnknownKey(t *testing.T) {
	if d := s.deferredFor(t, newName(), "never-seen"); d != 0 {
		t.Fatalf("DeferredFor(unknown) = %s, want 0", d)
	}
}

func (s storeAcceptance) deferUnsegmented(t *testing.T) {
	name := newName()
	if err := s.store.Defer(t.Context(), name, "", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	s.wantPaused(t, name, "", 5*time.Second)
}

// --- Status ---

func (s storeAcceptance) loadStatuses(t *testing.T, name string) []automation.Status {
	t.Helper()
	sts, err := s.store.LoadStatuses(t.Context(), name)
	if err != nil {
		t.Fatalf("LoadStatuses: %v", err)
	}
	return sts
}

func (s storeAcceptance) pauseIsPerAutomation(t *testing.T) {
	ctx := t.Context()
	n1, n2 := newName(), newName()
	if err := s.store.SetPaused(ctx, n1, true); err != nil {
		t.Fatalf("SetPaused: %v", err)
	}
	if p, err := s.store.Paused(ctx, n1); err != nil || !p {
		t.Fatalf("Paused(%s) = %v, %v; want true", n1, p, err)
	}
	if p, err := s.store.Paused(ctx, n2); err != nil || p {
		t.Fatalf("Paused(%s) = %v, %v; want false: a pause is per automation", n2, p, err)
	}
	if err := s.store.SetPaused(ctx, n1, false); err != nil {
		t.Fatalf("SetPaused(false): %v", err)
	}
	if p, err := s.store.Paused(ctx, n1); err != nil || p {
		t.Fatalf("Paused after resume = %v, %v; want false", p, err)
	}
}

func (s storeAcceptance) statusesSaved(t *testing.T) {
	ctx := t.Context()
	name := newName()
	b := automation.Status{Owner: "owner-b", Phase: automation.Waiting, WaitingFor: "todo-list", Processed: 7, Failed: 2, Deferred: 1, LastError: "boom"}
	a := automation.Status{Owner: "owner-a", Phase: automation.Working, Processed: 1}
	if err := s.store.SaveStatus(ctx, name, b, longTTL); err != nil {
		t.Fatalf("SaveStatus: %v", err)
	}
	if err := s.store.SaveStatus(ctx, name, automation.Status{Owner: "owner-a", Phase: automation.Idle}, longTTL); err != nil {
		t.Fatalf("SaveStatus: %v", err)
	}
	// A second save replaces the first.
	if err := s.store.SaveStatus(ctx, name, a, longTTL); err != nil {
		t.Fatalf("SaveStatus: %v", err)
	}
	sts := s.loadStatuses(t, name)
	if len(sts) != 2 {
		t.Fatalf("statuses = %+v, want owner-a and owner-b", sts)
	}
	for i, want := range []automation.Status{a, b} {
		got := sts[i]
		if got.Age < 0 || got.Age > pauseSlack {
			t.Fatalf("status of %s has age %s, want just saved", got.Owner, got.Age)
		}
		got.Age = 0
		if got != want {
			t.Fatalf("status %d = %+v, want %+v", i, got, want)
		}
	}
}

func (s storeAcceptance) statusesDropExpired(t *testing.T) {
	ctx := t.Context()
	name := newName()
	const ttl = 300 * time.Millisecond
	if err := s.store.SaveStatus(ctx, name, automation.Status{Owner: "gone", Phase: automation.Idle}, ttl); err != nil {
		t.Fatalf("SaveStatus: %v", err)
	}
	if err := s.store.SaveStatus(ctx, name, automation.Status{Owner: "here", Phase: automation.Idle}, longTTL); err != nil {
		t.Fatalf("SaveStatus: %v", err)
	}
	time.Sleep(2 * ttl)
	if sts := s.loadStatuses(t, name); len(sts) != 1 || sts[0].Owner != "here" {
		t.Fatalf("statuses = %+v, want only the one still live", sts)
	}
}

func (s storeAcceptance) segmentsLoaded(t *testing.T) {
	ctx := t.Context()
	name := newName()
	if r := s.acquire(t, name, "o1", []automation.Segment{{Key: "a", Group: "acme"}}, 1, longTTL); keysOf(r.Granted) != "a" {
		t.Fatalf("o1 got %q, want a", keysOf(r.Granted))
	}
	// b is only known from a deferral.
	if err := s.store.Defer(ctx, name, "b", time.Minute); err != nil {
		t.Fatal(err)
	}
	segs, err := s.store.LoadSegments(ctx, name)
	if err != nil {
		t.Fatalf("LoadSegments: %v", err)
	}
	if len(segs) != 2 {
		t.Fatalf("segments = %+v, want a and b", segs)
	}
	a, b := segs[0], segs[1]
	if a.Key != "a" || a.Group != "acme" || a.Owner != "o1" || a.PausedFor != 0 {
		t.Fatalf("segment a = %+v, want group acme, held by o1, not paused", a)
	}
	if b.Key != "b" || b.Owner != "" || b.PausedFor > time.Minute || b.PausedFor < time.Minute-pauseSlack {
		t.Fatalf("segment b = %+v, want unheld and paused for about a minute", b)
	}
}

func (s storeAcceptance) statusUnknown(t *testing.T) {
	ctx := t.Context()
	name := newName()
	if p, err := s.store.Paused(ctx, name); err != nil || p {
		t.Fatalf("Paused(unknown) = %v, %v; want false", p, err)
	}
	if sts := s.loadStatuses(t, name); len(sts) != 0 {
		t.Fatalf("LoadStatuses(unknown) = %+v, want none", sts)
	}
	if segs, err := s.store.LoadSegments(ctx, name); err != nil || len(segs) != 0 {
		t.Fatalf("LoadSegments(unknown) = %+v, %v; want none", segs, err)
	}
}
