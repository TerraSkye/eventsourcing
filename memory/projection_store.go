package memory

import (
	"cmp"
	"context"
	"slices"
	"sync"
	"time"

	"github.com/terraskye/eventsourcing/projection"
)

// ProjectionStore is a [projection.Store] that keeps everything in memory. Create one
// with [NewProjectionStore].
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
// Reset waits for every Commit in progress and holds off new ones while
// wipe runs, so no batch writes into the read model it wipes.
//
// It implements [projection.Store] fully, including Claim/Release fencing,
// so segmented projections and multiple goroutine instances of a runner can
// be exercised against it.
type ProjectionStore struct {
	// units is held for reading by every Commit, for its whole run, and for
	// writing by Reset, since neither can roll back what fn or wipe wrote.
	units sync.RWMutex

	mu          sync.Mutex
	projections map[string]*memoryProjection
	instances   map[string]map[string]*memoryInstance // by projection name, then owner
}

var (
	_ projection.Store           = (*ProjectionStore)(nil)
	_ projection.DeadLetterStore = (*ProjectionStore)(nil)
)

// NewProjectionStore returns an empty [ProjectionStore].
func NewProjectionStore() *ProjectionStore {
	return &ProjectionStore{
		projections: make(map[string]*memoryProjection),
		instances:   make(map[string]map[string]*memoryInstance),
	}
}

type memoryInstance struct {
	version   int
	heartbeat time.Time
	ttl       time.Duration // the instance's own, from its last heartbeat
}

type memoryProjection struct {
	segments int
	rows     map[int]*memoryRow

	// rebuildsRequested and rebuildsDone count RequestRebuild calls and
	// the requests Reset has served.
	rebuildsRequested uint64
	rebuildsDone      uint64
}

type memoryRow struct {
	cp          projection.Checkpoint
	status      projection.Status
	owner       string
	epoch       uint64
	heartbeat   time.Time
	ttl         time.Duration // the claim's own, from the Claim that took it
	deadLetters []projection.DeadLetter // ordered by Position
}

// Declare implements [projection.Store].
func (s *ProjectionStore) Declare(ctx context.Context, name string, segments int, replayUntil uint64) error {
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

// row returns the row for l's segment, or nil if Declare has not created it.
// s.mu must be held.
func (s *ProjectionStore) row(l projection.Lease) *memoryRow {
	p, ok := s.projections[l.Name]
	if !ok {
		return nil
	}
	return p.rows[l.Segment]
}

// owned returns the row for l's segment if l is its current claim, or nil.
// s.mu must be held.
func (s *ProjectionStore) owned(l projection.Lease) *memoryRow {
	row := s.row(l)
	if row == nil || row.owner == "" || row.owner != l.Owner || row.epoch != l.Epoch {
		return nil
	}
	return row
}

// Claim implements [projection.Store].
func (s *ProjectionStore) Claim(ctx context.Context, l projection.Lease, ttl time.Duration) (projection.Lease, projection.Checkpoint, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.row(l)
	if row == nil {
		return projection.Lease{}, projection.Checkpoint{}, false, nil
	}
	if s.owned(l) == nil {
		if row.owner != "" && time.Since(row.heartbeat) <= row.ttl {
			return projection.Lease{}, projection.Checkpoint{}, false, nil
		}
		row.epoch++
	}
	row.owner = l.Owner
	row.heartbeat = time.Now()
	row.ttl = ttl
	row.status.Owner = l.Owner
	l.Epoch = row.epoch
	return l, row.cp, true, nil
}

// Commit implements [projection.Store]. It cannot roll back fn's writes;
// see [ProjectionStore].
func (s *ProjectionStore) Commit(ctx context.Context, l projection.Lease, cp projection.Checkpoint, status projection.Status, fn func(ctx context.Context) error) error {
	s.units.RLock()
	defer s.units.RUnlock()

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
// see [ProjectionStore].
func (s *ProjectionStore) Reset(ctx context.Context, l projection.Lease, newVersion int, wipe func(ctx context.Context) error) error {
	s.units.Lock()
	defer s.units.Unlock()

	s.mu.Lock()
	held := s.owned(l) != nil
	s.mu.Unlock()
	if !held {
		return projection.ErrClaimLost
	}

	if err := wipe(ctx); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// A Claim does not wait for units, so l may have been taken over while
	// wipe ran.
	if s.owned(l) == nil {
		return projection.ErrClaimLost
	}
	p := s.projections[l.Name]
	p.rebuildsDone = p.rebuildsRequested
	for seg, row := range p.rows {
		row.deadLetters = nil
		row.cp = projection.Checkpoint{Position: 0, Version: newVersion, ReplayUntil: max(row.cp.ReplayUntil, row.cp.Position)}
		row.status.Position = 0
		row.status.Version = newVersion
		row.status.Head = 0
		row.status.LastError = ""
		if seg == l.Segment {
			row.status.Phase = projection.Starting
			row.heartbeat = time.Now()
			continue
		}
		row.owner = ""
		row.status.Owner = ""
		row.status.Phase = projection.Standby
	}
	return nil
}

// Release implements [projection.Store].
func (s *ProjectionStore) Release(ctx context.Context, l projection.Lease) error {
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
func (s *ProjectionStore) SaveStatus(ctx context.Context, l projection.Lease, status projection.Status) error {
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
func (s *ProjectionStore) LoadStatuses(ctx context.Context, name string) ([]projection.Status, error) {
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
		if row.owner != "" {
			st.ClaimTTL = row.ttl
		}
		st.ReplayUntil = row.cp.ReplayUntil
		st.RebuildRequested = p.rebuildsRequested > p.rebuildsDone
		out = append(out, st)
	}
	return out, nil
}

// RequestRebuild implements [projection.Store].
func (s *ProjectionStore) RequestRebuild(ctx context.Context, name string) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projections[name]
	if !ok {
		return 0, projection.ErrUnknownProjection
	}
	p.rebuildsRequested++
	return p.rebuildsRequested, nil
}

// Rebuilds implements [projection.Store].
func (s *ProjectionStore) Rebuilds(ctx context.Context, name string) (requested, done uint64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.projections[name]; ok {
		return p.rebuildsRequested, p.rebuildsDone, nil
	}
	return 0, 0, nil
}

// SaveDeadLetter implements [projection.DeadLetterStore]. Like Commit, it
// cannot be rolled back: a Commit that fails after fn recorded a dead letter
// keeps it.
func (s *ProjectionStore) SaveDeadLetter(ctx context.Context, l projection.Lease, d projection.DeadLetter) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.row(l)
	if row == nil {
		return projection.ErrClaimLost
	}
	i, _ := slices.BinarySearchFunc(row.deadLetters, d.Position, func(e projection.DeadLetter, pos uint64) int {
		return cmp.Compare(e.Position, pos)
	})
	row.deadLetters = slices.Insert(row.deadLetters, i, d)
	return nil
}

// LoadDeadLetters implements [projection.DeadLetterStore].
func (s *ProjectionStore) LoadDeadLetters(ctx context.Context, name string, segment int) ([]projection.DeadLetter, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.row(projection.Lease{Name: name, Segment: segment})
	if row == nil {
		return nil, nil
	}
	return slices.Clone(row.deadLetters), nil
}

// DeleteDeadLetter implements [projection.DeadLetterStore].
func (s *ProjectionStore) DeleteDeadLetter(ctx context.Context, l projection.Lease, position uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.row(l)
	if row == nil {
		return projection.ErrClaimLost
	}
	row.deadLetters = slices.DeleteFunc(row.deadLetters, func(d projection.DeadLetter) bool {
		return d.Position == position
	})
	return nil
}

// Heartbeat implements [projection.Store].
func (s *ProjectionStore) Heartbeat(ctx context.Context, name, owner string, version int, ttl time.Duration) ([]projection.Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	instances := s.instances[name]
	if ttl == 0 {
		delete(instances, owner)
		return nil, nil
	}
	if instances == nil {
		instances = make(map[string]*memoryInstance)
		s.instances[name] = instances
	}
	now := time.Now()
	instances[owner] = &memoryInstance{version: version, heartbeat: now, ttl: ttl}

	held := make(map[string]int)
	if p, ok := s.projections[name]; ok {
		for _, row := range p.rows {
			if row.owner != "" {
				held[row.owner]++
			}
		}
	}
	out := make([]projection.Instance, 0, len(instances))
	for o, m := range instances {
		age := now.Sub(m.heartbeat)
		if age > m.ttl {
			delete(instances, o) // expired by its own ttl
			continue
		}
		out = append(out, projection.Instance{Owner: o, Version: m.version, Segments: held[o], Age: age})
	}
	slices.SortFunc(out, func(a, b projection.Instance) int { return cmp.Compare(a.Owner, b.Owner) })
	return out, nil
}
