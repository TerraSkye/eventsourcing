// Package memory is an in-memory [projection.Store].
package memory

import (
	"context"
	"sync"
	"time"

	"github.com/terraskye/eventsourcing/projection"
)

// Store is a [projection.Store] that keeps everything in memory. Create one
// with [NewStore].
//
// It is intended for tests, single-process in-memory read models, and the
// documentation examples. It cannot roll back fn's writes on Commit (there
// is no database transaction), so projections built on it get at-least-once
// delivery: a handler that panics halfway through a batch may apply some of
// the batch's events twice when the runner retries. Handlers used with it
// must be safe to apply twice.
//
// Commit does not lock the segment while fn runs, so if a batch takes
// longer than the claim's ttl another instance can take the segment over
// halfway; the Commit then returns [projection.ErrClaimLost] after fn's
// writes were applied.
//
// It implements [projection.Store] fully, including Claim/Release fencing,
// so segmented projections and multiple goroutine instances of a runner can
// be exercised against it.
type Store struct {
	mu          sync.Mutex
	projections map[string]*memoryProjection
}

var _ projection.Store = (*Store)(nil)

// NewStore returns an empty [Store].
func NewStore() *Store {
	return &Store{projections: make(map[string]*memoryProjection)}
}

type memoryProjection struct {
	segments int
	rows     map[int]*memoryRow
}

type memoryRow struct {
	cp        projection.Checkpoint
	status    projection.Status
	owner     string
	epoch     uint64
	heartbeat time.Time
}

// Init implements [projection.Store].
func (s *Store) Init(ctx context.Context, name string, segments int, replayUntil uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projections[name]
	if !ok {
		rows := make(map[int]*memoryRow, segments)
		for i := range segments {
			rows[i] = &memoryRow{
				cp:     projection.Checkpoint{ReplayUntil: replayUntil},
				status: projection.Status{Phase: projection.Starting, Segment: i},
			}
		}
		s.projections[name] = &memoryProjection{segments: segments, rows: rows}
		return nil
	}
	if p.segments != segments {
		return projection.ErrSegmentsChanged
	}
	return nil
}

// row returns the row for l's segment, or nil if Init has not created it.
// s.mu must be held.
func (s *Store) row(l projection.Lease) *memoryRow {
	p, ok := s.projections[l.Name]
	if !ok {
		return nil
	}
	return p.rows[l.Segment]
}

// owned returns the row for l's segment if l is its current claim, or nil.
// s.mu must be held.
func (s *Store) owned(l projection.Lease) *memoryRow {
	row := s.row(l)
	if row == nil || row.owner == "" || row.owner != l.Owner || row.epoch != l.Epoch {
		return nil
	}
	return row
}

// Claim implements [projection.Store].
func (s *Store) Claim(ctx context.Context, l projection.Lease, ttl time.Duration) (projection.Lease, projection.Checkpoint, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.row(l)
	if row == nil {
		return projection.Lease{}, projection.Checkpoint{}, false, nil
	}
	if s.owned(l) == nil {
		if row.owner != "" && time.Since(row.heartbeat) <= ttl {
			return projection.Lease{}, projection.Checkpoint{}, false, nil
		}
		row.epoch++
	}
	row.owner = l.Owner
	row.heartbeat = time.Now()
	row.status.Owner = l.Owner
	l.Epoch = row.epoch
	return l, row.cp, true, nil
}

// Commit implements [projection.Store]. It cannot roll back fn's writes;
// see [Store].
func (s *Store) Commit(ctx context.Context, l projection.Lease, cp projection.Checkpoint, status projection.Status, fn func(ctx context.Context) error) error {
	// Check ownership before fn, so a handler never writes against a lost
	// claim, and again after fn, so a reclaim during fn is still reported.
	s.mu.Lock()
	held := s.owned(l) != nil
	s.mu.Unlock()
	if !held {
		return projection.ErrClaimLost
	}

	if err := fn(ctx); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.owned(l)
	if row == nil {
		return projection.ErrClaimLost
	}
	row.cp = cp
	row.status = status
	row.heartbeat = time.Now()
	return nil
}

// Reset implements [projection.Store]. It cannot roll back wipe's writes;
// see [Store].
func (s *Store) Reset(ctx context.Context, l projection.Lease, newVersion int, wipe func(ctx context.Context) error) error {
	s.mu.Lock()
	row := s.owned(l)
	if row == nil {
		s.mu.Unlock()
		return projection.ErrClaimLost
	}
	oldPos := row.cp.Position
	s.mu.Unlock()

	if err := wipe(ctx); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	row = s.owned(l)
	if row == nil {
		return projection.ErrClaimLost
	}
	row.cp = projection.Checkpoint{Position: 0, Version: newVersion, ReplayUntil: oldPos}
	row.status.Position = 0
	row.status.Version = newVersion
	row.heartbeat = time.Now()
	return nil
}

// Release implements [projection.Store].
func (s *Store) Release(ctx context.Context, l projection.Lease) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.owned(l)
	if row == nil {
		return nil
	}
	row.owner = ""
	row.status.Owner = ""
	row.status.Phase = projection.Standby
	return nil
}

// SaveStatus implements [projection.Store].
func (s *Store) SaveStatus(ctx context.Context, l projection.Lease, status projection.Status) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.owned(l)
	if row == nil {
		return projection.ErrClaimLost
	}
	// Progress only moves with the checkpoint; SaveStatus publishes the
	// rest.
	row.status.Phase = status.Phase
	row.status.Head = status.Head
	row.status.LastError = status.LastError
	row.heartbeat = time.Now()
	return nil
}

// LoadStatuses implements [projection.Store].
func (s *Store) LoadStatuses(ctx context.Context, name string) ([]projection.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projections[name]
	if !ok {
		return nil, nil
	}
	out := make([]projection.Status, 0, p.segments)
	for i := range p.segments {
		row := p.rows[i]
		if row == nil {
			continue
		}
		st := row.status
		st.Segment = i
		st.Position = row.cp.Position
		st.Version = row.cp.Version
		st.Owner = row.owner
		if !row.heartbeat.IsZero() {
			st.Age = time.Since(row.heartbeat)
		}
		out = append(out, st)
	}
	return out, nil
}
