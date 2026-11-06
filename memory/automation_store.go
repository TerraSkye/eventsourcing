package memory

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/terraskye/eventsourcing/automation"
)

var _ automation.Store = (*AutomationStore)(nil)

// AutomationStore is an in-process [automation.Store]: leases, pauses and
// fairness in memory, no table. It is the store for tests and for an
// automation that runs in one process; a second process would not see its
// leases, so use the sqlite or postgres AutomationStore when several
// processes run the automation. Create it with [NewAutomationStore] and
// pass it as [automation.Definition.Store].
type AutomationStore struct {
	mu        sync.Mutex
	rows      map[string]map[string]*localRow // name → key → row
	paused    map[string]bool                 // name → paused
	instances map[string]map[string]published // name → owner → status
}

// published is an instance's last published status.
type published struct {
	status automation.Status
	at     time.Time
	ttl    time.Duration
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

// NewAutomationStore returns an empty [AutomationStore].
func NewAutomationStore() *AutomationStore {
	return &AutomationStore{
		rows:      map[string]map[string]*localRow{},
		paused:    map[string]bool{},
		instances: map[string]map[string]published{},
	}
}

// row returns the row for (name, key), creating it at first sight.
func (s *AutomationStore) row(name, key string, now time.Time) *localRow {
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

// Acquire implements [automation.Store].
func (s *AutomationStore) Acquire(_ context.Context, name, owner string, candidates []automation.Segment, max int, ttl time.Duration) (automation.AcquireResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()

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

	var res automation.AcquireResult
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
			res.Granted = append(res.Granted, automation.SegmentLease{Name: name, Key: c.key, Owner: owner, Epoch: r.epoch})
			continue
		}
		res.Eligible++
	}
	return res, nil
}

// Renew implements [automation.Store].
func (s *AutomationStore) Renew(_ context.Context, held []automation.SegmentLease, ttl time.Duration) ([]automation.SegmentLease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var lost []automation.SegmentLease
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

// Release implements [automation.Store].
func (s *AutomationStore) Release(_ context.Context, rel []automation.ReleaseRequest) ([]automation.SegmentLease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var lost []automation.SegmentLease
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

// Defer implements [automation.Store].
func (s *AutomationStore) Defer(_ context.Context, name, key string, d time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	r := s.row(name, key, now)
	if t := now.Add(d); t.After(r.pausedTill) {
		r.pausedTill = t
	}
	return nil
}

// DeferredFor implements [automation.Store].
func (s *AutomationStore) DeferredFor(_ context.Context, name, key string) (time.Duration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.rows[name][key]
	if r == nil {
		return 0, nil
	}
	if d := time.Until(r.pausedTill); d > 0 {
		return d, nil
	}
	return 0, nil
}

// SetPaused implements [automation.Store].
func (s *AutomationStore) SetPaused(_ context.Context, name string, paused bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paused[name] = paused
	return nil
}

// Paused implements [automation.Store].
func (s *AutomationStore) Paused(_ context.Context, name string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paused[name], nil
}

// SaveStatus implements [automation.Store]. It also forgets the name's
// expired statuses.
func (s *AutomationStore) SaveStatus(_ context.Context, name string, status automation.Status, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	inst := s.instances[name]
	if inst == nil {
		inst = map[string]published{}
		s.instances[name] = inst
	}
	for o, p := range inst {
		if now.Sub(p.at) > p.ttl {
			delete(inst, o)
		}
	}
	status.Age = 0
	inst[status.Owner] = published{status: status, at: now, ttl: ttl}
	return nil
}

// LoadStatuses implements [automation.Store].
func (s *AutomationStore) LoadStatuses(_ context.Context, name string) ([]automation.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	out := []automation.Status{}
	for _, p := range s.instances[name] {
		if age := now.Sub(p.at); age <= p.ttl {
			st := p.status
			st.Age = age
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Owner < out[j].Owner })
	return out, nil
}

// LoadSegments implements [automation.Store].
func (s *AutomationStore) LoadSegments(_ context.Context, name string) ([]automation.SegmentState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	out := []automation.SegmentState{}
	for key, r := range s.rows[name] {
		seg := automation.SegmentState{Key: key, Group: r.group}
		if r.heldAt(now) {
			seg.Owner = r.owner
		}
		if d := r.pausedTill.Sub(now); d > 0 {
			seg.PausedFor = d
		}
		out = append(out, seg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}
