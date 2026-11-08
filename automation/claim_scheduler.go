package automation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// claimScheduler is the default [Scheduler]: it claims from the
// [TodoList]. For a plain list it is a competing consumer, as every
// instance claims independently. For a [Segmented] list it leases segments
// through a Store so that each segment has one owner at a time, and claims
// every held segment in one ClaimSegments call.
//
// Batches run on their own (see [Scheduler]), so a held segment is busy
// from the Next that hands out its batch until the batch's Drained: it is
// renewed but neither claimed for again nor rotated out meanwhile. A busy
// segment whose lease is lost stays busy too: it is not taken again until
// its batch drains, so that one segment never has two batches running.
type claimScheduler[T any] struct {
	name      string
	list      TodoList[T]
	segmented Segmented[T] // nil for a plain list
	segmenter Segmenter    // nil for a plain list
	store     Store
	logger    *slog.Logger

	lease        time.Duration
	ttl          time.Duration // segment lease; renewed on every Next
	maxPerKey    int
	maxSegments  int
	term         time.Duration // how long a segment is kept while others wait
	pollInterval time.Duration // idle churn guard: an empty segment is not re-acquired sooner
	now          func() time.Time

	owner string

	// mu guards the state below: Next, Done and Drained come from
	// different goroutines.
	mu           sync.Mutex
	held         map[string]*heldSegment
	lastEligible int
	busy         bool // unsegmented: a batch is running
}

type heldSegment struct {
	lease  SegmentLease
	since  time.Time
	served bool

	// busy is set while the segment's batch runs. leaseEnd is when the
	// batch's item leases end; deferUntil is the latest pause a Done of
	// the batch asked for, applied when the batch drains.
	busy       bool
	leaseEnd   time.Time
	deferUntil time.Time

	// lost is set when the lease was lost while the batch runs. The
	// segment is then ours no more, but stays busy until the batch drains:
	// not renewed, not acquired again and not claimed for meanwhile.
	lost bool
}

// newClaimScheduler returns the scheduler for d, leasing segments as owner.
func newClaimScheduler[T any](d Definition[T], owner string) *claimScheduler[T] {
	s := &claimScheduler[T]{
		name:         d.Name,
		list:         d.TodoList,
		store:        d.Store,
		logger:       d.Logger,
		lease:        d.Lease,
		ttl:          3*d.Lease + d.PollInterval,
		maxPerKey:    d.BatchSize,
		maxSegments:  d.MaxSegments,
		term:         DefaultSegmentTerm,
		pollInterval: d.PollInterval,
		now:          time.Now,
		owner:        owner,
		held:         map[string]*heldSegment{},
	}
	if seg, ok := any(d.TodoList).(Segmented[T]); ok {
		s.segmented = seg
		s.segmenter = seg.Segments()
	}
	return s
}

// capabilities describes what the scheduler detected on the list, for the
// startup log.
func (s *claimScheduler[T]) capabilities() string {
	if s.segmenter == nil {
		return "unsegmented"
	}
	return fmt.Sprintf("segmented (%s)", describe(s.segmenter))
}

func describe(seg Segmenter) string {
	if n, ok := seg.(shards); ok {
		return fmt.Sprintf("%d shards", int(n))
	}
	return "query"
}

// Next implements [Scheduler].
func (s *claimScheduler[T]) Next(ctx context.Context) ([]Batch[T], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.segmenter == nil {
		return s.nextUnsegmented(ctx)
	}
	return s.nextSegmented(ctx)
}

func (s *claimScheduler[T]) nextUnsegmented(ctx context.Context) ([]Batch[T], error) {
	if s.busy {
		return nil, nil
	}
	paused, err := s.store.DeferredFor(ctx, s.name, "")
	if err != nil {
		return nil, fmt.Errorf("deferred: %w", err)
	}
	if paused > 0 {
		return nil, nil
	}
	claimed := s.now()
	items, err := s.list.Claim(ctx, ClaimRequest{MaxPerKey: s.maxPerKey, Lease: s.lease})
	if err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}
	if len(items) == 0 {
		return nil, nil
	}
	b := Batch[T]{Items: make([]Item[T], len(items))}
	for i, it := range items {
		b.Items[i] = Item[T]{Value: it, Deadline: claimed.Add(s.lease)}
	}
	s.busy = true
	return []Batch[T]{b}, nil
}

func (s *claimScheduler[T]) nextSegmented(ctx context.Context) ([]Batch[T], error) {
	now := s.now()

	// Keep what we hold, and learn what we lost. A lost segment whose
	// batch still runs is kept, as lost, until the batch drains: taking
	// it again now would claim a second batch for it.
	held := make([]SegmentLease, 0, len(s.held))
	for _, h := range s.held {
		if !h.lost {
			held = append(held, h.lease)
		}
	}
	if len(held) > 0 {
		lost, err := s.store.Renew(ctx, held, s.ttl)
		if err != nil {
			return nil, fmt.Errorf("renew: %w", err)
		}
		for _, l := range lost {
			h := s.held[l.Key]
			if h != nil && h.busy {
				h.lost = true
			} else {
				delete(s.held, l.Key)
			}
			s.logger.LogAttrs(ctx, slog.LevelWarn, "automation segment lost",
				slog.String("automation", s.name), slog.String("segment", l.Key))
			// A pause the lost batch asked for must still reach the other
			// instances; Defer is not fenced.
			if h != nil && !h.deferUntil.IsZero() {
				if err := s.store.Defer(ctx, s.name, l.Key, h.deferUntil.Sub(now)); err != nil {
					return nil, fmt.Errorf("defer %s: %w", l.Key, err)
				}
			}
		}
	}

	// Rotation: when others are waiting and we are at our cap, give back
	// the segment we have held longest past the term. Work-conserving:
	// nothing is released while no segment is waiting, and a segment
	// whose batch still runs is not released at all.
	if s.lastEligible > 0 && len(s.held) >= s.maxSegments {
		var oldest *heldSegment
		for _, h := range s.held {
			if !h.busy && now.Sub(h.since) >= s.term && (oldest == nil || h.since.Before(oldest.since)) {
				oldest = h
			}
		}
		if oldest != nil {
			if err := s.release(ctx, oldest, 0); err != nil {
				return nil, err
			}
		}
	}

	// Discover and acquire. A lost segment whose batch still runs is not
	// a candidate: it keeps its slot, and is taken again at the earliest
	// on the Next after its Drained.
	cands, err := s.segmenter.segments(ctx)
	if err != nil {
		return nil, fmt.Errorf("segments: %w", err)
	}
	cands = s.withoutLost(cands)
	room := s.maxSegments - len(s.held)
	if room < 0 {
		room = 0
	}
	res, err := s.store.Acquire(ctx, s.name, s.owner, cands, room, s.ttl)
	if err != nil {
		return nil, fmt.Errorf("acquire: %w", err)
	}
	s.lastEligible = res.Eligible
	for _, l := range res.Granted {
		s.held[l.Key] = &heldSegment{lease: l, since: now}
	}
	var free []string
	for k, h := range s.held {
		if !h.busy {
			free = append(free, k)
		}
	}
	if len(free) == 0 {
		return nil, nil
	}

	batches, err := s.claimHeld(ctx, free)
	if err != nil {
		return nil, err
	}

	// A held segment with nothing to claim is idle: give it back now, so
	// the slot goes to a segment that has work, and do not take it again
	// before the next poll (the churn guard: with Shards, most buckets are
	// empty most of the time).
	got := map[string]time.Time{}
	for _, b := range batches {
		got[b.Key] = b.Items[0].Deadline
	}
	var served []*heldSegment
	for _, k := range free {
		h := s.held[k]
		if _, ok := got[k]; ok {
			served = append(served, h)
			continue
		}
		if err := s.release(ctx, h, s.pollInterval); err != nil {
			return nil, err
		}
	}

	// The batches are handed out only now that nothing can fail any more.
	// A segment marked busy for a batch that an error dropped would never
	// drain, so it would be renewed forever and never claimed for or
	// rotated again; held and free, it is claimed for again on the next
	// Next instead.
	for _, h := range served {
		h.served = true
		h.busy = true
		h.leaseEnd = got[h.lease.Key]
	}
	return batches, nil
}

// withoutLost returns cands without the segments held as lost.
func (s *claimScheduler[T]) withoutLost(cands []Segment) []Segment {
	out := cands[:0:0]
	for _, c := range cands {
		if h, ok := s.held[c.Key]; ok && h.lost {
			continue
		}
		out = append(out, c)
	}
	return out
}

// claimHeld claims for the held segments keys in one call.
func (s *claimScheduler[T]) claimHeld(ctx context.Context, keys []string) ([]Batch[T], error) {
	want := make(map[string]bool, len(keys))
	for _, k := range keys {
		want[k] = true
	}
	deadline := s.now().Add(s.lease)
	groups, err := s.segmented.ClaimSegments(ctx, ClaimRequest{Segments: keys, MaxPerKey: s.maxPerKey, Lease: s.lease})
	if err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}
	var batches []Batch[T]
	for _, g := range groups {
		if !want[g.Segment] {
			s.logger.LogAttrs(ctx, slog.LevelWarn, "automation claim returned a segment not asked for; skipping",
				slog.String("automation", s.name), slog.String("segment", g.Segment))
			continue
		}
		if len(g.Items) == 0 {
			continue
		}
		b := Batch[T]{Key: g.Segment, Items: make([]Item[T], len(g.Items))}
		for i, it := range g.Items {
			b.Items[i] = Item[T]{Key: g.Segment, Value: it, Deadline: deadline}
		}
		batches = append(batches, b)
	}
	return batches, nil
}

// release gives h back, paused for deferFor, and forgets it.
func (s *claimScheduler[T]) release(ctx context.Context, h *heldSegment, deferFor time.Duration) error {
	delete(s.held, h.lease.Key)
	_, err := s.store.Release(ctx, []ReleaseRequest{{Lease: h.lease, DeferFor: deferFor, Served: h.served}})
	if err != nil {
		return fmt.Errorf("release %s: %w", h.lease.Key, err)
	}
	return nil
}

// Done implements [Scheduler]. Only a deferral changes scheduler state.
// Unsegmented, the pause is written at once, so other instances stop
// claiming. Segmented, it is noted and applied when the batch drains: the
// segment stays ours until then, so nobody else can work it meanwhile.
func (s *claimScheduler[T]) Done(ctx context.Context, item Item[T], out Outcome) error {
	if out.Kind != DeferredOutcome {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	until := out.Until
	if !until.After(s.now()) {
		until = s.now().Add(s.lease)
	}
	if s.segmenter == nil {
		if err := s.store.Defer(ctx, s.name, "", until.Sub(s.now())); err != nil {
			return fmt.Errorf("defer: %w", err)
		}
		return nil
	}
	h, ok := s.held[item.Key]
	if !ok || h.lost {
		// Not ours any more; the pause must still be recorded.
		if err := s.store.Defer(ctx, s.name, item.Key, until.Sub(s.now())); err != nil {
			return fmt.Errorf("defer %s: %w", item.Key, err)
		}
		return nil
	}
	if until.After(h.deferUntil) {
		h.deferUntil = until
	}
	return nil
}

// Drained implements [Scheduler]. A segment that deferred is given back,
// paused until the later of the deferral and the end of its batch's item
// leases: items the batch did not start stay leased until then, and a
// later item of their chain must not be claimed elsewhere before them.
// Any other segment stays held and is claimed for again on the next Next.
func (s *claimScheduler[T]) Drained(ctx context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.segmenter == nil {
		s.busy = false
		return nil
	}
	h, ok := s.held[key]
	if !ok {
		return nil // lost meanwhile; Next recorded any pause
	}
	if h.lost {
		// Lost while the batch ran; Next recorded any pause, and the
		// segment may be taken again from the next Next on.
		delete(s.held, key)
		return nil
	}
	h.busy = false
	if h.deferUntil.IsZero() {
		return nil
	}
	until := h.deferUntil
	if h.leaseEnd.After(until) {
		until = h.leaseEnd
	}
	if err := s.release(ctx, h, until.Sub(s.now())); err != nil {
		// The segment stays the store's until its lease runs out, but the
		// pause must still reach whoever takes it then; Defer is not
		// fenced, as for a lost lease.
		if derr := s.store.Defer(ctx, s.name, key, until.Sub(s.now())); derr != nil {
			return errors.Join(err, fmt.Errorf("defer %s: %w", key, derr))
		}
		return err
	}
	return nil
}

// Close implements [Scheduler]: it gives back every held segment, with any
// pause a batch asked for. A lost segment is not ours to give back; Next
// recorded its pause when it was lost.
func (s *claimScheduler[T]) Close(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	rel := make([]ReleaseRequest, 0, len(s.held))
	for _, h := range s.held {
		if h.lost {
			continue
		}
		rr := ReleaseRequest{Lease: h.lease, Served: h.served}
		if h.deferUntil.After(now) {
			rr.DeferFor = h.deferUntil.Sub(now)
		}
		rel = append(rel, rr)
	}
	s.held = map[string]*heldSegment{}
	if len(rel) == 0 {
		return nil
	}
	if _, err := s.store.Release(ctx, rel); err != nil {
		return fmt.Errorf("release: %w", err)
	}
	return nil
}
