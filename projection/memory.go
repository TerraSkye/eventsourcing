package projection

import (
	"context"
	"sync"
	"time"
)

// NewMemoryStore returns a [Store] that keeps everything in memory.
//
// It is intended for tests, single-process in-memory read models, and the
// documentation examples. It cannot roll back fn's writes on Commit (there
// is no database transaction), so projections built on it get at-least-once
// delivery: a handler that panics halfway through a batch may apply some of
// the batch's events twice when the runner retries. Handlers used with it
// must be safe to apply twice.
//
// It implements [Store] fully, including Claim/Release fencing, so segmented
// projections and multiple goroutine instances of a runner can be exercised
// against it.
func NewMemoryStore() Store {
	return &memoryStore{projections: make(map[string]*memoryProjection)}
}

type memoryStore struct {
	mu          sync.Mutex
	projections map[string]*memoryProjection
}

type memoryProjection struct {
	segments int
	rows     map[int]*memoryRow
}

type memoryRow struct {
	cp         Checkpoint
	status     Status
	owner      string
	heartbeat  time.Time
}

func (s *memoryStore) Init(ctx context.Context, name string, segments int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projections[name]
	if !ok {
		rows := make(map[int]*memoryRow, segments)
		for i := 0; i < segments; i++ {
			rows[i] = &memoryRow{status: Status{Phase: Starting, Segment: i}}
		}
		s.projections[name] = &memoryProjection{segments: segments, rows: rows}
		return nil
	}
	if p.segments != segments {
		return ErrSegmentsChanged
	}
	return nil
}

func (s *memoryStore) Claim(ctx context.Context, name string, segment int, owner string, ttl time.Duration) (Checkpoint, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projections[name]
	if !ok {
		return Checkpoint{}, false, nil
	}
	row := p.rows[segment]
	if row == nil {
		return Checkpoint{}, false, nil
	}
	free := row.owner == "" || row.owner == owner || time.Since(row.heartbeat) > ttl
	if !free {
		return Checkpoint{}, false, nil
	}
	row.owner = owner
	row.heartbeat = time.Now()
	row.status.Owner = owner
	return row.cp, true, nil
}

func (s *memoryStore) Commit(ctx context.Context, name string, segment int, owner string, cp Checkpoint, status Status, fn func(ctx context.Context) error) error {
	// Pre-check ownership before fn, so a handler never writes against a
	// lost claim. Re-check after fn, so a reclaim during fn still rolls
	// back.
	s.mu.Lock()
	p, ok := s.projections[name]
	if !ok || p.rows[segment] == nil || p.rows[segment].owner != owner {
		s.mu.Unlock()
		return ErrClaimLost
	}
	s.mu.Unlock()

	if err := fn(ctx); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok = s.projections[name]
	if !ok {
		return ErrClaimLost
	}
	row := p.rows[segment]
	if row == nil || row.owner != owner {
		return ErrClaimLost
	}
	row.cp = cp
	row.status = status
	row.heartbeat = time.Now()
	return nil
}

func (s *memoryStore) Reset(ctx context.Context, name string, segment int, owner string, newVersion int, wipe func(ctx context.Context) error) error {
	s.mu.Lock()
	p, ok := s.projections[name]
	if !ok || p.rows[segment] == nil || p.rows[segment].owner != owner {
		s.mu.Unlock()
		return ErrClaimLost
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
		return ErrClaimLost
	}
	row := p.rows[segment]
	if row == nil || row.owner != owner {
		return ErrClaimLost
	}
	row.cp = Checkpoint{Position: 0, Version: newVersion, ReplayUntil: oldPos}
	row.status.Position = 0
	row.status.Version = newVersion
	row.heartbeat = time.Now()
	return nil
}

func (s *memoryStore) Release(ctx context.Context, name string, segment int, owner string) error {
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

func (s *memoryStore) SaveStatus(ctx context.Context, name string, segment int, owner string, st Status) error {
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
	row.status = st
	row.heartbeat = time.Now()
	return nil
}

func (s *memoryStore) LoadStatuses(ctx context.Context, name string) ([]Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projections[name]
	if !ok {
		return nil, nil
	}
	out := make([]Status, 0, p.segments)
	for i := 0; i < p.segments; i++ {
		row := p.rows[i]
		if row == nil {
			continue
		}
		st := row.status
		st.Segment = i
		if !row.heartbeat.IsZero() {
			st.Age = time.Since(row.heartbeat)
		}
		out = append(out, st)
	}
	return out, nil
}
