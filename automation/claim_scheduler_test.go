package automation

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// clock is a fake clock for the scheduler tests.
type clock struct{ t time.Time }

func newClock() *clock                { return &clock{t: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)} }
func (c *clock) now() time.Time       { return c.t }
func (c *clock) tick(d time.Duration) { c.t = c.t.Add(d) }

// fakeStore is a [Store] on a fake clock for the scheduler tests. The real
// stores live in packages that import this one, so these tests cannot use
// them. It hands out the key that has waited longest first and ignores
// groups; automationtest.StoreAcceptanceTest covers the full contract on
// the real stores.
type fakeStore struct {
	c    *clock
	rows map[string]*fakeRow // name + "/" + key
}

type fakeRow struct {
	owner      string
	epoch      uint64
	leaseUntil time.Time
	pausedTill time.Time
	waited     time.Time // last served, else first seen
}

func newFakeStore(c *clock) *fakeStore { return &fakeStore{c: c, rows: map[string]*fakeRow{}} }

func (s *fakeStore) row(name, key string) *fakeRow {
	r := s.rows[name+"/"+key]
	if r == nil {
		r = &fakeRow{waited: s.c.now()}
		s.rows[name+"/"+key] = r
	}
	return r
}

func (s *fakeStore) Acquire(_ context.Context, name, owner string, candidates []Segment, max int, ttl time.Duration) (AcquireResult, error) {
	now := s.c.now()
	var keys []string
	for _, c := range candidates {
		s.row(name, c.Key)
		keys = append(keys, c.Key)
	}
	sort.SliceStable(keys, func(i, j int) bool {
		wi, wj := s.row(name, keys[i]).waited, s.row(name, keys[j]).waited
		if !wi.Equal(wj) {
			return wi.Before(wj)
		}
		return keys[i] < keys[j]
	})
	var res AcquireResult
	for _, k := range keys {
		r := s.row(name, k)
		if r.pausedTill.After(now) {
			continue
		}
		res.Active++
		if r.owner != "" && r.leaseUntil.After(now) {
			continue
		}
		if len(res.Granted) == max {
			res.Eligible++
			continue
		}
		r.owner, r.leaseUntil = owner, now.Add(ttl)
		r.epoch++
		res.Granted = append(res.Granted, SegmentLease{Name: name, Key: k, Owner: owner, Epoch: r.epoch})
	}
	return res, nil
}

func (s *fakeStore) Renew(_ context.Context, held []SegmentLease, ttl time.Duration) ([]SegmentLease, error) {
	var lost []SegmentLease
	for _, l := range held {
		r := s.rows[l.Name+"/"+l.Key]
		if r == nil || r.owner != l.Owner || r.epoch != l.Epoch || !r.leaseUntil.After(s.c.now()) {
			lost = append(lost, l)
			continue
		}
		r.leaseUntil = s.c.now().Add(ttl)
	}
	return lost, nil
}

func (s *fakeStore) Release(_ context.Context, rel []ReleaseRequest) ([]SegmentLease, error) {
	var lost []SegmentLease
	for _, rr := range rel {
		r := s.rows[rr.Lease.Name+"/"+rr.Lease.Key]
		if r == nil || r.owner != rr.Lease.Owner || r.epoch != rr.Lease.Epoch {
			lost = append(lost, rr.Lease)
			continue
		}
		r.owner, r.leaseUntil = "", time.Time{}
		if t := s.c.now().Add(rr.DeferFor); rr.DeferFor > 0 && t.After(r.pausedTill) {
			r.pausedTill = t
		}
		if rr.Served {
			r.waited = s.c.now()
		}
	}
	return lost, nil
}

func (s *fakeStore) Defer(_ context.Context, name, key string, d time.Duration) error {
	r := s.row(name, key)
	if t := s.c.now().Add(d); t.After(r.pausedTill) {
		r.pausedTill = t
	}
	return nil
}

func (s *fakeStore) DeferredFor(_ context.Context, name, key string) (time.Duration, error) {
	r := s.rows[name+"/"+key]
	if r == nil || !r.pausedTill.After(s.c.now()) {
		return 0, nil
	}
	return r.pausedTill.Sub(s.c.now()), nil
}

// The pause and status methods are not used by the scheduler.
func (s *fakeStore) SetPaused(context.Context, string, bool) error { return nil }
func (s *fakeStore) Paused(context.Context, string) (bool, error)  { return false, nil }
func (s *fakeStore) SaveStatus(context.Context, string, Status, time.Duration) error {
	return nil
}
func (s *fakeStore) LoadStatuses(context.Context, string) ([]Status, error) { return nil, nil }
func (s *fakeStore) LoadSegments(context.Context, string) ([]SegmentState, error) {
	return nil, nil
}

// newTestScheduler returns the scheduler for d, as instance "test-owner".
func newTestScheduler(d Definition[string]) *claimScheduler[string] {
	return newClaimScheduler(d, "test-owner")
}

// owner returns who holds key, or "" when nobody does.
func (s *fakeStore) owner(name, key string) string {
	r := s.rows[name+"/"+key]
	if r == nil || !r.leaseUntil.After(s.c.now()) {
		return ""
	}
	return r.owner
}

// TestClaimScheduler_RotatesUnderContention covers the work-conserving
// rotation rule: with more keys than an instance may hold, a segment
// held past the term is given back when another key is waiting, and the
// waiting key gets the next turn.
func TestClaimScheduler_RotatesUnderContention(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	store := newFakeStore(c)

	// Two segments with endless work; the scheduler may hold one.
	s := newTestScheduler(Definition[string]{
		Name:        "sync",
		TodoList:    endlessList{"a", "b"},
		Store:       store,
		Logger:      quietLogger(),
		Lease:       time.Minute,
		BatchSize:   1,
		MaxSegments: 1,
	})
	s.now = c.now
	s.term = 10 * time.Second

	var served []string
	for range 4 {
		batches, err := s.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(batches) != 1 {
			t.Fatalf("got %d batches, want 1 (MaxSegments)", len(batches))
		}
		served = append(served, batches[0].Key)
		for _, it := range batches[0].Items {
			if err := s.Done(ctx, it, Outcome{Kind: Done}); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Drained(ctx, batches[0].Key); err != nil {
			t.Fatal(err)
		}
		c.tick(11 * time.Second) // past the term
	}
	if got := strings.Join(served, ","); got != "a,b,a,b" {
		t.Fatalf("served %s, want a,b,a,b: the held key must rotate out while the other waits", got)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if store.owner("sync", "a") != "" || store.owner("sync", "b") != "" {
		t.Fatal("Close did not release the held segment")
	}
}

// TestClaimScheduler_KeepsAnUncontendedSegment covers the other half of
// the rule: with nothing waiting, a held key is never rotated out.
func TestClaimScheduler_KeepsAnUncontendedSegment(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	store := newFakeStore(c)
	s := newTestScheduler(Definition[string]{
		Name:        "sync",
		TodoList:    endlessList{"a"},
		Store:       store,
		Logger:      quietLogger(),
		Lease:       time.Minute,
		BatchSize:   1,
		MaxSegments: 1,
	})
	s.now = c.now
	s.term = 10 * time.Second
	var epoch uint64
	for i := range 3 {
		batches, err := s.Next(ctx)
		if err != nil || len(batches) != 1 {
			t.Fatalf("round %d: %v %d batches", i, err, len(batches))
		}
		e := s.held["a"].lease.Epoch
		if i > 0 && e != epoch {
			t.Fatalf("round %d: epoch %d, want %d; the uncontended key was re-acquired", i, e, epoch)
		}
		epoch = e
		if err := s.Drained(ctx, "a"); err != nil {
			t.Fatal(err)
		}
		c.tick(11 * time.Second)
	}
}

func quietLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// endlessList is a Query-segmented list where every segment always has one
// more item.
type endlessList []string

func (l endlessList) Claim(_ context.Context, req ClaimRequest) ([]string, error) {
	var out []string
	for _, k := range req.Segments {
		out = append(out, k+"-item")
	}
	return out, nil
}

func (l endlessList) Segments() Segmenter {
	return Query(func(context.Context) ([]Segment, error) {
		segs := make([]Segment, len(l))
		for i, k := range l {
			segs[i] = Segment{Key: k}
		}
		return segs, nil
	})
}

func (l endlessList) ClaimSegments(_ context.Context, req ClaimRequest) ([]Claimed[string], error) {
	out := make([]Claimed[string], 0, len(req.Segments))
	for _, k := range req.Segments {
		out = append(out, Claimed[string]{Segment: k, Items: []string{k + "-item"}})
	}
	return out, nil
}

// plainList has only Claim: the default, competing consumers.
type plainList struct{}

func (plainList) Claim(context.Context, ClaimRequest) ([]string, error) { return nil, nil }

// shardedList is Segmented by Shards. It records every ClaimRequest it
// gets and has endless work in the shards named in work only.
type shardedList struct {
	n    int
	work []string
	reqs []ClaimRequest
}

func (l *shardedList) Claim(context.Context, ClaimRequest) ([]string, error) { return nil, nil }

func (l *shardedList) ClaimSegments(_ context.Context, req ClaimRequest) ([]Claimed[string], error) {
	l.reqs = append(l.reqs, req)
	var out []Claimed[string]
	for _, k := range req.Segments {
		if slices.Contains(l.work, k) {
			out = append(out, Claimed[string]{Segment: k, Items: []string{"item-in-" + k}})
		}
	}
	return out, nil
}

func (l *shardedList) Segments() Segmenter { return Shards(l.n) }

func TestClaimScheduler_DetectsCapabilities(t *testing.T) {
	base := Definition[string]{Name: "x", Store: newFakeStore(newClock()), Logger: quietLogger(), Lease: time.Minute, BatchSize: 1, MaxSegments: 4}
	cases := map[string]struct {
		list TodoList[string]
		want string
	}{
		"plain":  {plainList{}, "unsegmented"},
		"query":  {endlessList{"a"}, "segmented (query)"},
		"shards": {&shardedList{n: 16}, "segmented (16 shards)"},
	}
	for name, c := range cases {
		d := base
		d.TodoList = c.list
		if got := newTestScheduler(d).capabilities(); got != c.want {
			t.Errorf("%s: capabilities = %q, want %q", name, got, c.want)
		}
	}
}

// TestClaimScheduler_ShardsClaimInOneCallAndGuardIdleChurn covers that
// all held shards go into one ClaimSegments call, and the churn guard: an
// empty shard, once released, is not acquired again before the poll
// interval has passed.
func TestClaimScheduler_ShardsClaimInOneCallAndGuardIdleChurn(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	store := newFakeStore(c)
	list := &shardedList{n: 4, work: []string{"3"}}
	s := newTestScheduler(Definition[string]{
		Name:         "sync",
		TodoList:     list,
		Store:        store,
		Logger:       quietLogger(),
		Lease:        time.Minute,
		BatchSize:    5,
		MaxSegments:  4,
		PollInterval: 10 * time.Second,
	})
	s.now = c.now

	batches, err := s.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 1 || batches[0].Key != "3" || len(batches[0].Items) != 1 {
		t.Fatalf("batches = %+v, want one item from shard 3", batches)
	}
	if len(list.reqs) != 1 || len(list.reqs[0].Segments) != 4 {
		t.Fatalf("claim requests %+v, want one call naming all four held shards", list.reqs)
	}

	// The three empty shards were released and paused for PollInterval;
	// shard 3 is still held. A poll right away must not re-acquire them.
	for _, k := range []string{"0", "1", "2"} {
		if store.owner("sync", k) != "" {
			t.Fatalf("empty shard %s still held", k)
		}
		if d, _ := store.DeferredFor(ctx, "sync", k); d != 10*time.Second {
			t.Fatalf("empty shard %s paused for %s, want the poll interval", k, d)
		}
	}
	if store.owner("sync", "3") == "" {
		t.Fatal("shard 3 with work was released")
	}
	if err := s.Drained(ctx, "3"); err != nil {
		t.Fatal(err)
	}
	list.reqs = nil
	c.tick(time.Second)
	if _, err := s.Next(ctx); err != nil {
		t.Fatal(err)
	}
	if len(list.reqs) != 1 || len(list.reqs[0].Segments) != 1 || list.reqs[0].Segments[0] != "3" {
		t.Fatalf("second poll claimed %v, want only shard 3 while the others are paused", list.reqs)
	}
	if err := s.Drained(ctx, "3"); err != nil {
		t.Fatal(err)
	}

	// After the interval the empty shards are candidates again.
	c.tick(10 * time.Second)
	list.reqs = nil
	if _, err := s.Next(ctx); err != nil {
		t.Fatal(err)
	}
	if len(list.reqs) != 1 || len(list.reqs[0].Segments) != 4 {
		t.Fatalf("third poll claimed %+v, want all 4 shards again", list.reqs)
	}
}

// TestClaimScheduler_ShardsRotateOnlyForABucketWithWork covers rotation
// with Shards, where every bucket is a candidate whether it has work or
// not: at its cap, past the term, the scheduler claims one waiting bucket
// before it gives a held segment back. An empty bucket is given back
// instead and the held segment keeps its lease; a bucket with work takes
// the held segment's place.
func TestClaimScheduler_ShardsRotateOnlyForABucketWithWork(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	store := newFakeStore(c)
	list := &shardedList{n: 4, work: []string{"0"}}
	s := newTestScheduler(Definition[string]{
		Name: "sync", TodoList: list, Store: store, Logger: quietLogger(),
		Lease: time.Minute, BatchSize: 1, MaxSegments: 1, PollInterval: 10 * time.Second,
	})
	s.now = c.now
	s.term = 10 * time.Second

	// A round is one Next after tick: the keys it served, and the claim
	// calls it made, each as its keys.
	type round struct{ served, claimed string }
	run := func(tick time.Duration) round {
		t.Helper()
		c.tick(tick)
		list.reqs = nil
		batches, err := s.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var r round
		for _, b := range batches {
			r.served += b.Key
			s.Drained(ctx, b.Key)
		}
		var calls []string
		for _, req := range list.reqs {
			calls = append(calls, strings.Join(req.Segments, ""))
		}
		r.claimed = strings.Join(calls, ",")
		return r
	}
	check := func(step string, got, want round) {
		t.Helper()
		if got != want {
			t.Fatalf("%s: served %q with claims %q, want %q with %q", step, got.served, got.claimed, want.served, want.claimed)
		}
	}

	check("first poll", run(0), round{"0", "0"})
	epoch := s.held["0"].lease.Epoch

	// Past the term, the three empty buckets are eligible as far as the
	// store knows. Each Next claims one of them before shard 0: empty, it
	// is given back, paused for the poll interval, and shard 0 keeps its
	// lease. Once all three are paused nothing is waiting any more.
	check("probe 1", run(11*time.Second), round{"0", "1,0"})
	check("probe 2", run(time.Second), round{"0", "2,0"})
	check("probe 3", run(time.Second), round{"0", "3,0"})
	check("nothing waiting", run(time.Second), round{"0", "0"})
	if e := s.held["0"].lease.Epoch; e != epoch {
		t.Fatalf("shard 0 has epoch %d, want %d: it was given back for an empty bucket", e, epoch)
	}
	for _, k := range []string{"1", "2", "3"} {
		if store.owner("sync", k) != "" {
			t.Fatalf("empty shard %s is held", k)
		}
	}

	// Work appears in shard 3. The next poll finds the empty buckets'
	// pauses over, so they are looked at again, in order; shard 3, once
	// claimed, takes shard 0's place.
	list.work = append(list.work, "3")
	check("next poll", run(10*time.Second), round{"0", "0"})
	check("probe 1 again", run(time.Second), round{"0", "1,0"})
	check("probe 2 again", run(time.Second), round{"0", "2,0"})
	check("shard 3 has work", run(time.Second), round{"3", "3"})
	if store.owner("sync", "0") != "" {
		t.Fatal("shard 0 was not given back for shard 3")
	}

	// Shard 0 now waits with work, behind the empty buckets in the
	// fairness order. Once shard 3 has held past the term it gets the
	// same treatment: the empty buckets are claimed and given back, then
	// shard 0 takes its place.
	check("probe 1 for shard 3", run(11*time.Second), round{"3", "1,3"})
	check("probe 2 for shard 3", run(time.Second), round{"3", "2,3"})
	check("shard 0 takes over", run(time.Second), round{"0", "0"})
	if store.owner("sync", "3") != "" {
		t.Fatal("shard 3 was not given back for shard 0")
	}
}

// flakyStore is a fakeStore whose Release fails the next failures times.
type flakyStore struct {
	*fakeStore
	failures int
}

func (s *flakyStore) Release(ctx context.Context, rel []ReleaseRequest) ([]SegmentLease, error) {
	if s.failures > 0 {
		s.failures--
		return nil, errors.New("store down")
	}
	return s.fakeStore.Release(ctx, rel)
}

// TestClaimScheduler_FailedIdleReleaseKeepsSegmentsWorkable covers a Next
// that fails while giving back an empty shard: the batches it drops must
// not leave the shards with work marked busy, as their Drained never
// comes, so the next Next claims for them again.
func TestClaimScheduler_FailedIdleReleaseKeepsSegmentsWorkable(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	store := &flakyStore{fakeStore: newFakeStore(c), failures: 1}
	s := newTestScheduler(Definition[string]{
		Name: "sync", TodoList: &shardedList{n: 4, work: []string{"3"}}, Store: store, Logger: quietLogger(),
		Lease: time.Minute, BatchSize: 5, MaxSegments: 4, PollInterval: 10 * time.Second,
	})
	s.now = c.now

	if batches, err := s.Next(ctx); err == nil {
		t.Fatalf("Next = %d batches, nil; want the release error", len(batches))
	}
	if h := s.held["3"]; h == nil || h.busy {
		t.Fatalf("shard 3 after the failed Next: %+v; want held and not busy, its batch was dropped", h)
	}
	for i := range 2 {
		c.tick(time.Second)
		batches, err := s.Next(ctx)
		if err != nil {
			t.Fatalf("Next %d after the failure: %v", i+1, err)
		}
		if len(batches) != 1 || batches[0].Key != "3" {
			t.Fatalf("Next %d after the failure = %+v, want the batch of shard 3", i+1, batches)
		}
		if err := s.Drained(ctx, "3"); err != nil {
			t.Fatal(err)
		}
	}
}

// TestClaimScheduler_DeferralReleasesAtDrained covers a deferral while its
// batch runs: the segment stays ours, is not claimed for again, and is
// given back only when the batch drains, paused until the later of the
// deferral and the end of the batch's item leases.
func TestClaimScheduler_DeferralReleasesAtDrained(t *testing.T) {
	for name, c := range map[string]struct {
		deferFor, wantPause time.Duration
	}{
		"lease ends later":    {deferFor: 10 * time.Second, wantPause: time.Minute},
		"deferral ends later": {deferFor: 5 * time.Minute, wantPause: 5 * time.Minute},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			clk := newClock()
			store := newFakeStore(clk)
			s := newTestScheduler(Definition[string]{
				Name: "sync", TodoList: endlessList{"a"}, Store: store, Logger: quietLogger(),
				Lease: time.Minute, BatchSize: 1, MaxSegments: 1, PollInterval: time.Second,
			})
			s.now = clk.now

			batches, err := s.Next(ctx)
			if err != nil || len(batches) != 1 {
				t.Fatalf("Next = %d batches, %v; want one", len(batches), err)
			}
			if err := s.Done(ctx, batches[0].Items[0], Outcome{Kind: DeferredOutcome, Until: clk.now().Add(c.deferFor)}); err != nil {
				t.Fatal(err)
			}
			if store.owner("sync", "a") == "" {
				t.Fatal("the segment was given back before its batch drained")
			}
			if again, err := s.Next(ctx); err != nil || len(again) != 0 {
				t.Fatalf("Next while the batch runs = %d batches, %v; want none", len(again), err)
			}

			if err := s.Drained(ctx, "a"); err != nil {
				t.Fatal(err)
			}
			if store.owner("sync", "a") != "" {
				t.Fatal("the deferred segment is still held after its batch drained")
			}
			if d, _ := store.DeferredFor(ctx, "sync", "a"); d != c.wantPause {
				t.Fatalf("paused for %s, want %s", d, c.wantPause)
			}
		})
	}
}

// TestClaimScheduler_FailedReleaseAtDrainedKeepsItsDeferral covers a
// deferred batch whose release at Drained fails: the pause must still be
// written, so the instance that takes the segment over honours it.
func TestClaimScheduler_FailedReleaseAtDrainedKeepsItsDeferral(t *testing.T) {
	ctx := context.Background()
	clk := newClock()
	store := &flakyStore{fakeStore: newFakeStore(clk)}
	s := newTestScheduler(Definition[string]{
		Name: "sync", TodoList: endlessList{"a"}, Store: store, Logger: quietLogger(),
		Lease: time.Minute, BatchSize: 1, MaxSegments: 1, PollInterval: time.Second,
	})
	s.now = clk.now

	batches, err := s.Next(ctx)
	if err != nil || len(batches) != 1 {
		t.Fatalf("Next = %d batches, %v; want one", len(batches), err)
	}
	if err := s.Done(ctx, batches[0].Items[0], Outcome{Kind: DeferredOutcome, Until: clk.now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	store.failures = 1
	if err := s.Drained(ctx, "a"); err == nil {
		t.Fatal("Drained = nil, want the release error")
	}
	if d, _ := store.DeferredFor(ctx, "sync", "a"); d < 50*time.Minute {
		t.Fatalf("after the failed release the segment is paused for %s, want the deferral", d)
	}
}

// TestClaimScheduler_LostSegmentKeepsItsDeferral covers a segment whose
// lease is lost while a deferral waits for its batch to drain: the pause
// is written anyway, so the instance that takes the segment over honours
// it.
func TestClaimScheduler_LostSegmentKeepsItsDeferral(t *testing.T) {
	ctx := context.Background()
	clk := newClock()
	store := newFakeStore(clk)
	s := newTestScheduler(Definition[string]{
		Name: "sync", TodoList: endlessList{"a"}, Store: store, Logger: quietLogger(),
		Lease: time.Minute, BatchSize: 1, MaxSegments: 1, PollInterval: time.Second,
	})
	s.now = clk.now

	batches, err := s.Next(ctx)
	if err != nil || len(batches) != 1 {
		t.Fatalf("Next = %d batches, %v; want one", len(batches), err)
	}
	if err := s.Done(ctx, batches[0].Items[0], Outcome{Kind: DeferredOutcome, Until: clk.now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	clk.tick(s.ttl + time.Second) // the batch hangs past the segment lease
	if _, err := s.Next(ctx); err != nil {
		t.Fatal(err)
	}
	if d, _ := store.DeferredFor(ctx, "sync", "a"); d < 50*time.Minute {
		t.Fatalf("after the lease was lost the segment is paused for %s, want the deferral", d)
	}
	if err := s.Drained(ctx, "a"); err != nil {
		t.Fatalf("Drained of a lost segment: %v", err)
	}
}

// TestClaimScheduler_LostSegmentGetsNoSecondBatch covers a segment whose
// lease is lost while its batch still runs: the segment stays busy, so
// Next neither takes it again nor claims a second batch for it before the
// batch drains, which would run items of one chain twice at once. Once
// the batch has drained, the segment is a candidate again.
func TestClaimScheduler_LostSegmentGetsNoSecondBatch(t *testing.T) {
	ctx := context.Background()
	clk := newClock()
	store := newFakeStore(clk)
	s := newTestScheduler(Definition[string]{
		Name: "sync", TodoList: endlessList{"a"}, Store: store, Logger: quietLogger(),
		Lease: time.Minute, BatchSize: 1, MaxSegments: 1, PollInterval: time.Second,
	})
	s.now = clk.now

	batches, err := s.Next(ctx)
	if err != nil || len(batches) != 1 {
		t.Fatalf("Next = %d batches, %v; want one", len(batches), err)
	}
	clk.tick(s.ttl + time.Second) // the batch hangs past the segment lease
	again, err := s.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("Next after the lease was lost = %d batches, want none: the first batch of the segment is still running", len(again))
	}
	if store.owner("sync", "a") != "" {
		t.Fatal("the lost segment was taken again while its batch still runs")
	}

	if err := s.Drained(ctx, "a"); err != nil {
		t.Fatalf("Drained of a lost segment: %v", err)
	}
	batches, err = s.Next(ctx)
	if err != nil || len(batches) != 1 {
		t.Fatalf("Next after the batch drained = %d batches, %v; want one", len(batches), err)
	}
}

// itemsList is an unsegmented list that always has one more item.
type itemsList struct{}

func (itemsList) Claim(context.Context, ClaimRequest) ([]string, error) {
	return []string{"item"}, nil
}

// TestClaimScheduler_UnsegmentedOneBatchAtATime covers that a plain list
// gets no second batch while the first one runs.
func TestClaimScheduler_UnsegmentedOneBatchAtATime(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(Definition[string]{
		Name: "plain", TodoList: itemsList{}, Store: newFakeStore(newClock()), Logger: quietLogger(),
		Lease: time.Minute, BatchSize: 1,
	})
	if b, _ := s.Next(ctx); len(b) != 1 {
		t.Fatalf("first Next = %d batches, want one", len(b))
	}
	if b, _ := s.Next(ctx); len(b) != 0 {
		t.Fatalf("Next while the batch runs = %d batches, want none", len(b))
	}
	if err := s.Drained(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.Next(ctx); len(b) != 1 {
		t.Fatalf("Next after Drained = %d batches, want one", len(b))
	}
}
