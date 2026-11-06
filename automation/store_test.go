package automation

import (
	"context"
	"strings"
	"testing"
	"time"
)

// clock is a fake clock for the store and scheduler tests.
type clock struct{ t time.Time }

func newClock() *clock                { return &clock{t: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)} }
func (c *clock) now() time.Time       { return c.t }
func (c *clock) tick(d time.Duration) { c.t = c.t.Add(d) }

func keysOf(ls []SegmentLease) string {
	var ks []string
	for _, l := range ls {
		ks = append(ks, l.Key)
	}
	return strings.Join(ks, ",")
}

func TestLocalStore_RoundRobinsGroupsThenSegments(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	s := NewLocalStore()
	s.Now = c.now

	// acme has two keys seen first; zed has one key seen later.
	cands := []Segment{
		{Key: "acme:stripe", Group: "acme"},
		{Key: "acme:shopify", Group: "acme"},
	}
	if _, err := s.Acquire(ctx, "sync", "o1", cands, 0, time.Minute); err != nil {
		t.Fatal(err)
	}
	c.tick(time.Second)
	cands = append(cands, Segment{Key: "zed:stripe", Group: "zed"})

	// A multi-grant interleaves groups: acme's first key, zed's first,
	// then acme's second; never both acme keys before zed.
	res, err := s.Acquire(ctx, "sync", "o1", cands, 3, time.Minute)
	if err != nil || keysOf(res.Granted) != "acme:shopify,zed:stripe,acme:stripe" {
		t.Fatalf("multi-grant = %q (%v), want acme:shopify,zed:stripe,acme:stripe", keysOf(res.Granted), err)
	}
	var back []ReleaseRequest
	for _, l := range res.Granted {
		back = append(back, ReleaseRequest{Lease: l}) // not served: waits unchanged
	}
	s.Release(ctx, back)

	// One grant at a time, each served: tenants alternate, and acme's two
	// keys take turns inside acme's turns.
	var served []string
	for range 6 {
		res, err := s.Acquire(ctx, "sync", "o1", cands, 1, time.Minute)
		if err != nil || len(res.Granted) != 1 {
			t.Fatalf("grant: %v %v", res.Granted, err)
		}
		served = append(served, res.Granted[0].Key)
		c.tick(time.Second)
		s.Release(ctx, []ReleaseRequest{{Lease: res.Granted[0], Served: true}})
	}
	want := "acme:shopify,zed:stripe,acme:stripe,zed:stripe,acme:shopify,zed:stripe"
	if got := strings.Join(served, ","); got != want {
		t.Fatalf("served\n %s\nwant\n %s", got, want)
	}
	if res.Active != 3 {
		t.Fatalf("active %d, want 3", res.Active)
	}
}

func TestLocalStore_NeverGrantsAHeldKey(t *testing.T) {
	ctx := context.Background()
	s := NewLocalStore()
	cands := []Segment{{Key: "a"}, {Key: "b"}}
	r1, _ := s.Acquire(ctx, "sync", "o1", cands, 1, time.Minute)
	r2, _ := s.Acquire(ctx, "sync", "o2", cands, 2, time.Minute)
	if keysOf(r1.Granted) != "a" || keysOf(r2.Granted) != "b" {
		t.Fatalf("o1 got %q, o2 got %q; want a and b", keysOf(r1.Granted), keysOf(r2.Granted))
	}
	r3, _ := s.Acquire(ctx, "sync", "o3", cands, 2, time.Minute)
	if len(r3.Granted) != 0 || r3.Eligible != 0 || r3.Active != 2 {
		t.Fatalf("o3 = %+v, want nothing granted, nothing eligible, 2 active", r3)
	}
	if s.Owner("sync", "a") != "o1" || s.Owner("sync", "b") != "o2" {
		t.Fatal("owners not recorded")
	}
}

func TestLocalStore_FencesOnEpoch(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	s := NewLocalStore()
	s.Now = c.now
	cands := []Segment{{Key: "a"}}

	r1, _ := s.Acquire(ctx, "sync", "o1", cands, 1, time.Minute)
	old := r1.Granted[0]

	// o1 goes quiet: its lease expires and o2 takes the key.
	c.tick(2 * time.Minute)
	r2, _ := s.Acquire(ctx, "sync", "o2", cands, 1, time.Minute)
	if keysOf(r2.Granted) != "a" || r2.Granted[0].Epoch != old.Epoch+1 {
		t.Fatalf("o2 grant = %+v, want key a with epoch %d", r2.Granted, old.Epoch+1)
	}

	// o1's stale lease can neither renew nor release.
	if lost, _ := s.Renew(ctx, []SegmentLease{old}, time.Minute); keysOf(lost) != "a" {
		t.Fatalf("stale renew lost %q, want a", keysOf(lost))
	}
	if lost, _ := s.Release(ctx, []ReleaseRequest{{Lease: old, DeferFor: time.Hour}}); keysOf(lost) != "a" {
		t.Fatalf("stale release lost %q, want a", keysOf(lost))
	}
	if s.Owner("sync", "a") != "o2" {
		t.Fatal("stale release disturbed the holder")
	}
	if d, _ := s.DeferredFor(ctx, "sync", "a"); d != 0 {
		t.Fatal("stale release paused the key")
	}

	// The live lease renews and releases.
	if lost, _ := s.Renew(ctx, r2.Granted, time.Minute); len(lost) != 0 {
		t.Fatalf("live renew lost %v", lost)
	}
	if lost, _ := s.Release(ctx, []ReleaseRequest{{Lease: r2.Granted[0]}}); len(lost) != 0 {
		t.Fatalf("live release lost %v", lost)
	}
	if s.Owner("sync", "a") != "" {
		t.Fatal("key still owned after release")
	}
}

func TestLocalStore_PausesSkipGrants(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	s := NewLocalStore()
	s.Now = c.now
	cands := []Segment{{Key: "a"}, {Key: "b"}}

	if err := s.Defer(ctx, "sync", "a", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.Defer(ctx, "sync", "a", time.Second); err != nil { // shorter: keeps the longer pause
		t.Fatal(err)
	}
	if d, _ := s.DeferredFor(ctx, "sync", "a"); d != time.Minute {
		t.Fatalf("paused for %s, want 1m", d)
	}
	res, _ := s.Acquire(ctx, "sync", "o1", cands, 2, time.Minute)
	if keysOf(res.Granted) != "b" || res.Active != 1 {
		t.Fatalf("grant = %q active %d, want only b", keysOf(res.Granted), res.Active)
	}
	s.Release(ctx, []ReleaseRequest{{Lease: res.Granted[0]}})
	c.tick(time.Minute)
	res, _ = s.Acquire(ctx, "sync", "o1", cands, 1, time.Minute)
	if keysOf(res.Granted) != "a" {
		t.Fatalf("after the pause grant = %q, want a (it has waited longest)", keysOf(res.Granted))
	}

	// A release with DeferFor pauses too, and the unsegmented "" key
	// works like any other.
	s.Release(ctx, []ReleaseRequest{{Lease: res.Granted[0], DeferFor: 30 * time.Second}})
	if d, _ := s.DeferredFor(ctx, "sync", "a"); d != 30*time.Second {
		t.Fatalf("paused for %s after release, want 30s", d)
	}
	s.Defer(ctx, "sync", "", 5*time.Second)
	if d, _ := s.DeferredFor(ctx, "sync", ""); d != 5*time.Second {
		t.Fatalf("unsegmented pause %s, want 5s", d)
	}
}
