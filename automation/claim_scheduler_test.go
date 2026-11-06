package automation

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestClaimScheduler_RotatesUnderContention covers the work-conserving
// rotation rule: with more keys than an instance may hold, a segment
// held past the term is given back when another key is waiting, and the
// waiting key gets the next turn.
func TestClaimScheduler_RotatesUnderContention(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	store := NewLocalStore()
	store.Now = c.now

	// Two segments with endless work; the scheduler may hold one.
	s := newClaimScheduler(Definition[string]{
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
			s.Done(ctx, it, Outcome{Kind: Done})
		}
		s.Drained(ctx, batches[0].Key)
		c.tick(11 * time.Second) // past the term
	}
	if got := strings.Join(served, ","); got != "a,b,a,b" {
		t.Fatalf("served %s, want a,b,a,b: the held key must rotate out while the other waits", got)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if store.Owner("sync", "a") != "" || store.Owner("sync", "b") != "" {
		t.Fatal("Close did not release the held segment")
	}
}

// TestClaimScheduler_KeepsAnUncontendedSegment covers the other half of
// the rule: with nothing waiting, a held key is never rotated out.
func TestClaimScheduler_KeepsAnUncontendedSegment(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	store := NewLocalStore()
	store.Now = c.now
	s := newClaimScheduler(Definition[string]{
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
		s.Drained(ctx, "a")
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
// gets and has work in shard "3" only.
type shardedList struct {
	n    int
	reqs []ClaimRequest
}

func (l *shardedList) Claim(context.Context, ClaimRequest) ([]string, error) { return nil, nil }

func (l *shardedList) ClaimSegments(_ context.Context, req ClaimRequest) ([]Claimed[string], error) {
	l.reqs = append(l.reqs, req)
	for _, k := range req.Segments {
		if k == "3" {
			return []Claimed[string]{{Segment: "3", Items: []string{"item-in-3"}}}, nil
		}
	}
	return nil, nil
}

func (l *shardedList) Segments() Segmenter { return Shards(l.n) }

func TestClaimScheduler_DetectsCapabilities(t *testing.T) {
	base := Definition[string]{Name: "x", Store: NewLocalStore(), Logger: quietLogger(), Lease: time.Minute, BatchSize: 1, MaxSegments: 4}
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
		if got := newClaimScheduler(d).capabilities(); got != c.want {
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
	store := NewLocalStore()
	store.Now = c.now
	list := &shardedList{n: 4}
	s := newClaimScheduler(Definition[string]{
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
		if store.Owner("sync", k) != "" {
			t.Fatalf("empty shard %s still held", k)
		}
		if d, _ := store.DeferredFor(ctx, "sync", k); d != 10*time.Second {
			t.Fatalf("empty shard %s paused for %s, want the poll interval", k, d)
		}
	}
	if store.Owner("sync", "3") == "" {
		t.Fatal("shard 3 with work was released")
	}
	s.Drained(ctx, "3")
	list.reqs = nil
	c.tick(time.Second)
	if _, err := s.Next(ctx); err != nil {
		t.Fatal(err)
	}
	if len(list.reqs) != 1 || len(list.reqs[0].Segments) != 1 || list.reqs[0].Segments[0] != "3" {
		t.Fatalf("second poll claimed %v, want only shard 3 while the others are paused", list.reqs)
	}
	s.Drained(ctx, "3")

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
			store := NewLocalStore()
			store.Now = clk.now
			s := newClaimScheduler(Definition[string]{
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
			if store.Owner("sync", "a") == "" {
				t.Fatal("the segment was given back before its batch drained")
			}
			if again, err := s.Next(ctx); err != nil || len(again) != 0 {
				t.Fatalf("Next while the batch runs = %d batches, %v; want none", len(again), err)
			}

			if err := s.Drained(ctx, "a"); err != nil {
				t.Fatal(err)
			}
			if store.Owner("sync", "a") != "" {
				t.Fatal("the deferred segment is still held after its batch drained")
			}
			if d, _ := store.DeferredFor(ctx, "sync", "a"); d != c.wantPause {
				t.Fatalf("paused for %s, want %s", d, c.wantPause)
			}
		})
	}
}

// TestClaimScheduler_LostSegmentKeepsItsDeferral covers a segment whose
// lease is lost while a deferral waits for its batch to drain: the pause
// is written anyway, so the instance that takes the segment over honours
// it.
func TestClaimScheduler_LostSegmentKeepsItsDeferral(t *testing.T) {
	ctx := context.Background()
	clk := newClock()
	store := NewLocalStore()
	store.Now = clk.now
	s := newClaimScheduler(Definition[string]{
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

// itemsList is an unsegmented list that always has one more item.
type itemsList struct{}

func (itemsList) Claim(context.Context, ClaimRequest) ([]string, error) {
	return []string{"item"}, nil
}

// TestClaimScheduler_UnsegmentedOneBatchAtATime covers that a plain list
// gets no second batch while the first one runs.
func TestClaimScheduler_UnsegmentedOneBatchAtATime(t *testing.T) {
	ctx := context.Background()
	s := newClaimScheduler(Definition[string]{
		Name: "plain", TodoList: itemsList{}, Store: NewLocalStore(), Logger: quietLogger(),
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
