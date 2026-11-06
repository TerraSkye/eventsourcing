package automation

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Store keeps the segment leases of the default scheduler: which instance
// holds each key, until when, and when a key may run again. The library
// implements it for memory, SQLite and Postgres; [LocalStore] is the
// in-process one for single-process deployments and tests.
//
// Its methods and their signatures may change in minor releases before
// v1.0; external implementations should expect breakage.
type Store interface {
	// Acquire records candidates it has not seen, then grants up to max
	// eligible keys to owner for ttl. A key is eligible when nobody holds
	// a live lease on it and its pause has passed. Candidates are ordered
	// by the group that has waited longest first, then by the key that
	// has waited longest within it; keys with no group form one group.
	// Every grant raises the key's epoch. max may be 0: the call still
	// records candidates and reports Active and Eligible.
	Acquire(ctx context.Context, name, owner string, candidates []Segment, max int, ttl time.Duration) (AcquireResult, error)

	// Renew extends every lease in held by ttl and returns the ones the
	// owner no longer holds.
	Renew(ctx context.Context, held []SegmentLease, ttl time.Duration) (lost []SegmentLease, err error)

	// Release gives segments back, each fenced on owner and epoch. A
	// released key is paused for DeferFor (0 for none) and, when Served,
	// moves to the back of the fairness order. It returns the leases that
	// were lost; nothing changes for those.
	Release(ctx context.Context, rel []ReleaseRequest) (lost []SegmentLease, err error)

	// Defer pauses key for d from now, keeping any longer pause already
	// set. It is not fenced: a stale writer can only delay the next grant,
	// never disturb a holder. key is "" for an unsegmented automation.
	Defer(ctx context.Context, name, key string, d time.Duration) error

	// DeferredFor returns how long key is still paused, or 0.
	DeferredFor(ctx context.Context, name, key string) (time.Duration, error)
}

// SegmentLease identifies one grant of a segment to an owner. Epoch fences
// it: a later grant of the same key has a higher epoch, and operations
// carrying an older one are refused.
type SegmentLease struct {
	Name, Key, Owner string
	Epoch            uint64
}

// ReleaseRequest is one segment to give back; see [Store.Release].
type ReleaseRequest struct {
	Lease    SegmentLease
	DeferFor time.Duration
	Served   bool
}

// AcquireResult is what [Store.Acquire] granted and what it saw.
type AcquireResult struct {
	// Granted are the new leases, in grant order.
	Granted []SegmentLease

	// Active counts candidates whose pause has passed, held or not.
	Active int

	// Eligible counts candidates that nobody holds and whose pause has
	// passed, after this grant: keys that are waiting for an owner.
	Eligible int
}

// LocalStore is an in-process [Store]: per-process leases, pauses and
// fairness, no table. It is the right store for tests and for an
// automation that runs in one process; a second process would not see its
// leases. Use it through [Definition.Store], or let a nil Store default to
// it.
type LocalStore struct {
	// Now returns the current time. It is a field so that tests can run
	// the store on a fake clock; leave it nil for the real one.
	Now func() time.Time

	mu   sync.Mutex
	rows map[string]map[string]*localRow // name → key → row
}

type localRow struct {
	group      string
	owner      string
	epoch      uint64
	leaseUntil time.Time
	pausedTill time.Time
	firstSeen  time.Time
	lastServed time.Time // zero until the key has been served once
}

// NewLocalStore returns an empty [LocalStore].
func NewLocalStore() *LocalStore {
	return &LocalStore{rows: map[string]map[string]*localRow{}}
}

func (s *LocalStore) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// row returns the row for (name, key), creating it at first sight.
func (s *LocalStore) row(name, key string, now time.Time) *localRow {
	rows := s.rows[name]
	if rows == nil {
		rows = map[string]*localRow{}
		s.rows[name] = rows
	}
	r := rows[key]
	if r == nil {
		r = &localRow{firstSeen: now}
		rows[key] = r
	}
	return r
}

// waited is the instant since which a key has been waiting for its next
// turn: when it was last served, or when it was first seen.
func (r *localRow) waited() time.Time {
	if r.lastServed.IsZero() {
		return r.firstSeen
	}
	return r.lastServed
}

func (r *localRow) heldAt(now time.Time) bool { return r.owner != "" && r.leaseUntil.After(now) }

// Acquire implements [Store].
func (s *LocalStore) Acquire(_ context.Context, name, owner string, candidates []Segment, max int, ttl time.Duration) (AcquireResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()

	type cand struct {
		key string
		row *localRow
	}
	cands := make([]cand, 0, len(candidates))
	seen := map[string]bool{}
	for _, c := range candidates {
		if seen[c.Key] {
			continue
		}
		seen[c.Key] = true
		r := s.row(name, c.Key, now)
		if c.Group != "" {
			r.group = c.Group
		}
		cands = append(cands, cand{c.Key, r})
	}

	// Round-robin across groups, then across a group's keys. A group's
	// turn is as old as the last time any of its keys was served (or as
	// its first sight, if never); within a group, the key that has waited
	// longest goes first. Grants interleave groups: every group's first
	// key before any group's second, so a multi-grant does not hand one
	// tenant all the slots.
	groupTurn := map[string]time.Time{}
	byGroup := map[string][]int{}
	for i, c := range cands {
		g := c.row.group
		byGroup[g] = append(byGroup[g], i)
		if !c.row.lastServed.IsZero() {
			if t, ok := groupTurn[g]; !ok || c.row.lastServed.After(t) {
				groupTurn[g] = c.row.lastServed
			}
		}
	}
	for g, idx := range byGroup {
		if _, ok := groupTurn[g]; ok {
			continue
		}
		first := cands[idx[0]].row.firstSeen
		for _, i := range idx[1:] {
			if t := cands[i].row.firstSeen; t.Before(first) {
				first = t
			}
		}
		groupTurn[g] = first
	}
	rank := make([]int, len(cands))
	for _, idx := range byGroup {
		sort.SliceStable(idx, func(a, b int) bool {
			wa, wb := cands[idx[a]].row.waited(), cands[idx[b]].row.waited()
			if !wa.Equal(wb) {
				return wa.Before(wb)
			}
			return cands[idx[a]].key < cands[idx[b]].key
		})
		for r, i := range idx {
			rank[i] = r
		}
	}
	order := make([]int, len(cands))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		ia, ib := order[a], order[b]
		if rank[ia] != rank[ib] {
			return rank[ia] < rank[ib]
		}
		ga, gb := cands[ia].row.group, cands[ib].row.group
		ta, tb := groupTurn[ga], groupTurn[gb]
		if !ta.Equal(tb) {
			return ta.Before(tb)
		}
		if ga != gb {
			return ga < gb
		}
		return cands[ia].key < cands[ib].key
	})
	sorted := make([]cand, len(cands))
	for i, j := range order {
		sorted[i] = cands[j]
	}
	cands = sorted

	var res AcquireResult
	for _, c := range cands {
		r := c.row
		if r.pausedTill.After(now) {
			continue
		}
		res.Active++
		if r.heldAt(now) {
			continue
		}
		if len(res.Granted) < max {
			r.owner = owner
			r.epoch++
			r.leaseUntil = now.Add(ttl)
			res.Granted = append(res.Granted, SegmentLease{Name: name, Key: c.key, Owner: owner, Epoch: r.epoch})
			continue
		}
		res.Eligible++
	}
	return res, nil
}

// Renew implements [Store].
func (s *LocalStore) Renew(_ context.Context, held []SegmentLease, ttl time.Duration) ([]SegmentLease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	var lost []SegmentLease
	for _, l := range held {
		r := s.rows[l.Name][l.Key]
		if r == nil || r.owner != l.Owner || r.epoch != l.Epoch || !r.leaseUntil.After(now) {
			lost = append(lost, l)
			continue
		}
		r.leaseUntil = now.Add(ttl)
	}
	return lost, nil
}

// Release implements [Store].
func (s *LocalStore) Release(_ context.Context, rel []ReleaseRequest) ([]SegmentLease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	var lost []SegmentLease
	for _, rr := range rel {
		l := rr.Lease
		r := s.rows[l.Name][l.Key]
		if r == nil || r.owner != l.Owner || r.epoch != l.Epoch {
			lost = append(lost, l)
			continue
		}
		r.owner = ""
		r.leaseUntil = time.Time{}
		if rr.DeferFor > 0 {
			if t := now.Add(rr.DeferFor); t.After(r.pausedTill) {
				r.pausedTill = t
			}
		}
		if rr.Served {
			r.lastServed = now
		}
	}
	return lost, nil
}

// Defer implements [Store].
func (s *LocalStore) Defer(_ context.Context, name, key string, d time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	r := s.row(name, key, now)
	if t := now.Add(d); t.After(r.pausedTill) {
		r.pausedTill = t
	}
	return nil
}

// DeferredFor implements [Store].
func (s *LocalStore) DeferredFor(_ context.Context, name, key string) (time.Duration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.rows[name][key]
	if r == nil {
		return 0, nil
	}
	if d := r.pausedTill.Sub(s.now()); d > 0 {
		return d, nil
	}
	return 0, nil
}

// Owner returns who holds key, or "" when nobody does. For tests and
// dashboards.
func (s *LocalStore) Owner(name, key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.rows[name][key]
	if r == nil || !r.heldAt(s.now()) {
		return ""
	}
	return r.owner
}
