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
	heartbeat time.Time
}

func (s *Store) Init(ctx context.Context, name string, segments int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projections[name]
	if !ok {
		rows := make(map[int]*memoryRow, segments)
		for i := range segments {
			rows[i] = &memoryRow{status: projection.Status{Phase: projection.Starting, Segment: i}}
		}
		s.projections[name] = &memoryProjection{segments: segments, rows: rows}
		return nil
	}
	if p.segments != segments {
		return projection.ErrSegmentsChanged
	}
	return nil
}

func (s *Store) Claim(ctx context.Context, name string, segment int, owner string, ttl time.Duration) (projection.Checkpoint, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projections[name]
	if !ok {
		return projection.Checkpoint{}, false, nil
	}
	row := p.rows[segment]
	if row == nil {
		return projection.Checkpoint{}, false, nil
	}
	free := row.owner == "" || row.owner == owner || time.Since(row.heartbeat) > ttl
	if !free {
		return projection.Checkpoint{}, false, nil
	}
	row.owner = owner
	row.heartbeat = time.Now()
	row.status.Owner = owner
	return row.cp, true, nil
}

func (s *Store) Commit(ctx context.Context, name string, segment int, owner string, cp projection.Checkpoint, status projection.Status, fn func(ctx context.Context) error) error {
	// Pre-check ownership before fn, so a handler never writes against a
	// lost claim. Re-check after fn, so a reclaim during fn still rolls
	// back.
	s.mu.Lock()
	p, ok := s.projections[name]
	if !ok || p.rows[segment] == nil || p.rows[segment].owner != owner {
		s.mu.Unlock()
		return projection.ErrClaimLost
	}
	s.mu.Unlock()

	if err := fn(ctx); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok = s.projections[name]
	if !ok {
		return projection.ErrClaimLost
	}
	row := p.rows[segment]
	if row == nil || row.owner != owner {
		return projection.ErrClaimLost
	}
	row.cp = cp
	row.status = status
	row.heartbeat = time.Now()
	return nil
}

func (s *Store) Reset(ctx context.Context, name string, segment int, owner string, newVersion int, wipe func(ctx context.Context) error) error {
	s.mu.Lock()
	p, ok := s.projections[name]
	if !ok || p.rows[segment] == nil || p.rows[segment].owner != owner {
		s.mu.Unlock()
		return projection.ErrClaimLost
	}
	oldPos := p.rows[segment].cp.Position
	s.mu.Unlock()

	if err := wipe(ctx); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok = s.projections[name]
	if !ok {
		return projection.ErrClaimLost
	}
	row := p.rows[segment]
	if row == nil || row.owner != owner {
		return projection.ErrClaimLost
	}
	row.cp = projection.Checkpoint{Position: 0, Version: newVersion, ReplayUntil: oldPos}
	row.status.Position = 0
	row.status.Version = newVersion
	row.heartbeat = time.Now()
	return nil
}

func (s *Store) Release(ctx context.Context, name string, segment int, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projections[name]
	if !ok {
		return nil
	}
	row := p.rows[segment]
	if row == nil || row.owner != owner {
		return nil
	}
	row.owner = ""
	row.status.Owner = ""
	return nil
}

func (s *Store) SaveStatus(ctx context.Context, name string, segment int, owner string, st projection.Status) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projections[name]
	if !ok || p.rows[segment] == nil {
		return nil
	}
	row := p.rows[segment]
	if row.owner != owner {
		return nil
	}
	// Progress only moves with the checkpoint; SaveStatus publishes the
	// rest.
	row.status.Phase = st.Phase
	row.status.Head = st.Head
	row.status.LastError = st.LastError
	row.heartbeat = time.Now()
	return nil
}

func (s *Store) LoadStatuses(ctx context.Context, name string) ([]projection.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projections[name]
	if !ok {
		return nil, nil
	}
	out := make([]projection.Status, 0, p.segments)
	for i := 0; i < p.segments; i++ {
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
