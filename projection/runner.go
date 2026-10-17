package projection

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/terraskye/eventsourcing"
)

var (
	// ErrNotActive is returned by [Runner.Rebuild] when this instance is not
	// working the projection: Run has not started or has returned, or
	// another instance holds the claim. A rebuild only reaches the runner
	// in this process; bump [Projection.Version] to rebuild wherever the
	// projection runs.
	ErrNotActive = errors.New("projection: runner is not active on this instance")

	// ErrNoReset is returned by [Runner.Rebuild] when the projection has no
	// Reset function registered.
	ErrNoReset = errors.New("projection: rebuild requires a Reset function")

	// ErrSegmented is returned by [Runner.Rebuild] on a segmented projection.
	// A segmented projection is rebuilt by running it under a new name; see
	// [Projection.Version].
	ErrSegmented = errors.New("projection: segmented projections are rebuilt under a new name")
)

// Runner keeps one projection up to date: it claims the segments it can,
// reads the global log past each segment's checkpoint, routes each event
// through the projection's handlers, and commits the handlers' writes
// together with the new checkpoint.
//
// Create a Runner with [NewRunner] and start it with [Runner.Run]. All
// other methods are safe to call from any goroutine, before, during or
// after Run.
type Runner struct {
	p        Projection
	reader   eventsourcing.GlobalReader
	store    Store
	segments int // normalized: at least 1

	nudge chan struct{}

	mu             sync.Mutex
	segs           []*segmentState // indexed by segment
	changed        chan struct{}   // closed and replaced on every status change, for WaitUntil
	watchers       []func()
	pendingRebuild *rebuildRequest // taken by segment 0 between batches

	skipped atomic.Int64
}

type segmentState struct {
	status Status

	// active reports that this instance holds the segment's claim and is
	// processing it.
	active bool
}

// rebuildRequest is a pending [Runner.Rebuild]. The runner sets err and
// closes done once the rebuild has run, so every caller waiting on the same
// request sees the outcome.
type rebuildRequest struct {
	done chan struct{}
	err  error
}

func (req *rebuildRequest) finish(err error) {
	req.err = err
	close(req.done)
}

// NewRunner creates a Runner for p against reader and store.
//
// It panics on wiring mistakes that would otherwise fail silently: an empty
// Name, nil Handler/reader/store, no Events or an unregistered one,
// Version > 0 without Reset, or Segments > 1 without SegmentKey or with a
// Version.
func NewRunner(p Projection, reader eventsourcing.GlobalReader, store Store) *Runner {
	switch {
	case p.Name == "":
		panic("projection: Projection.Name is required")
	case p.Handler == nil:
		panic(fmt.Sprintf("projection %q: Handler is required", p.Name))
	case len(p.Events) == 0:
		panic(fmt.Sprintf("projection %q: Events is required", p.Name))
	case reader == nil:
		panic(fmt.Sprintf("projection %q: reader is required", p.Name))
	case store == nil:
		panic(fmt.Sprintf("projection %q: store is required", p.Name))
	}
	if p.Version != 0 && p.Reset == nil {
		panic(fmt.Sprintf("projection %q: Version > 0 requires Reset", p.Name))
	}
	segments := max(p.Segments, 1)
	if segments > 1 && p.SegmentKey == nil {
		panic(fmt.Sprintf("projection %q: Segments > 1 requires SegmentKey", p.Name))
	}
	if segments > 1 && p.Version != 0 {
		panic(fmt.Sprintf("projection %q: a segmented projection cannot have a Version; rebuild it under a new Name", p.Name))
	}
	for _, name := range p.Events {
		if _, err := eventsourcing.NewEventByName(name); err != nil {
			panic(fmt.Sprintf("projection %q: event %q is not registered; register it with eventsourcing.RegisterEvent", p.Name, name))
		}
	}

	if p.BatchSize <= 0 {
		p.BatchSize = DefaultBatchSize
	}
	if p.PollInterval <= 0 {
		p.PollInterval = DefaultPollInterval
	}
	if p.ClaimTTL <= 0 {
		p.ClaimTTL = DefaultClaimTTL
	}
	if p.Owner == "" {
		p.Owner = defaultOwner()
	}

	r := &Runner{
		p:        p,
		reader:   reader,
		store:    store,
		segments: segments,
		nudge:    make(chan struct{}, 1),
		segs:     make([]*segmentState, segments),
		changed:  make(chan struct{}),
	}
	for i := range r.segs {
		r.segs[i] = &segmentState{status: Status{Phase: Starting, Version: p.Version, Segment: i}}
	}
	return r
}

// defaultOwner returns "hostname/pid/random", with "unknown" for a hostname
// that cannot be read. The random part keeps two runners in one process
// apart, and a restarted process apart from the claims of the one before
// it: in containers the pid is usually the same after a restart, and Claim
// hands a segment back to the owner that already holds it.
func defaultOwner() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	var b [4]byte
	_, _ = rand.Read(b[:]) // never returns an error
	return host + "/" + strconv.Itoa(os.Getpid()) + "/" + hex.EncodeToString(b[:])
}

// Name returns the projection's name.
func (r *Runner) Name() string { return r.p.Name }

// Run processes events until ctx is done, then returns ctx.Err(). It
// returns early only when it cannot start: when reading the head of the log
// or [Store.Init] fails, with that error.
//
// Failures while processing never make Run return. When a batch fails the
// runner reports [Stalled], backs off and retries from its last committed
// checkpoint, until the handler succeeds or the projection is rebuilt.
//
// Each segment competes for its claim: on success Run loads the checkpoint
// and processes events; otherwise it reports [Standby] for that segment and
// keeps trying. If the stored version differs from [Projection.Version] on
// a segment, that segment is rebuilt first.
//
// Call Run once per Runner. On return it has released every claim it held.
func (r *Runner) Run(ctx context.Context) error {
	// Events already in the log when the projection is first created count
	// as replayed; see IsReplaying. Init only uses head for segments it
	// creates, so a restart keeps the boundary of the first start.
	head, err := r.reader.Head(ctx)
	if err != nil {
		return fmt.Errorf("projection %q: read head: %w", r.p.Name, err)
	}
	if err := r.store.Init(ctx, r.p.Name, r.segments, head); err != nil {
		return fmt.Errorf("projection %q: init: %w", r.p.Name, err)
	}

	// One loop per segment. The loops compete for the claims with the
	// loops of every other instance of this program.
	var wg sync.WaitGroup
	for seg := range r.segments {
		wg.Go(func() { r.runSegment(ctx, seg) })
	}
	wg.Wait()
	return ctx.Err()
}

// Rebuild wipes the read model and processes the log again from the start.
// The runner performs it between two batches, under the ctx passed to Run.
//
// ctx only bounds how long Rebuild waits: once requested, the rebuild runs
// even if ctx ends first. Concurrent calls share one rebuild.
//
// Rebuild returns [ErrSegmented] on a segmented projection, [ErrNoReset]
// when the projection has no Reset, and [ErrNotActive] when this instance
// is not processing the projection, or stops before the rebuild ran. To
// rebuild wherever the projection runs, bump [Projection.Version]; to
// rebuild a segmented projection, run it under a new Name.
func (r *Runner) Rebuild(ctx context.Context) error {
	if r.segments > 1 {
		return ErrSegmented
	}
	if r.p.Reset == nil {
		return ErrNoReset
	}

	r.mu.Lock()
	if !r.segs[0].active {
		r.mu.Unlock()
		return ErrNotActive
	}
	req := r.pendingRebuild
	if req == nil {
		req = &rebuildRequest{done: make(chan struct{})}
		r.pendingRebuild = req
	}
	r.mu.Unlock()
	r.Nudge()

	select {
	case <-req.done:
		return req.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// takeRebuild returns the pending rebuild request for seg, or nil, and
// clears it.
func (r *Runner) takeRebuild(seg int) *rebuildRequest {
	if r.segments > 1 || seg != 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	req := r.pendingRebuild
	r.pendingRebuild = nil
	return req
}

// setActive records whether this instance is processing seg. When seg stops
// being active, a pending rebuild fails with [ErrNotActive].
func (r *Runner) setActive(seg int, active bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.segs[seg].active = active
	if !active && seg == 0 && r.pendingRebuild != nil {
		r.pendingRebuild.finish(ErrNotActive)
		r.pendingRebuild = nil
	}
}

// Nudge asks the runner to read now instead of waiting for its next poll.
//
// Nudge never blocks, and nudges coalesce. Only reaches this process's
// runner; other instances rely on polling.
func (r *Runner) Nudge() {
	select {
	case r.nudge <- struct{}{}:
	default: // a nudge is already pending
	}
}

// Watch registers fn to be called after every committed batch that
// contained events, and on every change of [Phase]. It lets a component
// that depends on this projection learn of new work without polling.
//
// fn runs on the runner's goroutine and must not block; typically it is
// another component's Nudge method.
func (r *Runner) Watch(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.watchers = append(r.watchers, fn)
}

func (r *Runner) notifyWatchers() {
	r.mu.Lock()
	watchers := append([]func(){}, r.watchers...)
	r.mu.Unlock()
	for _, fn := range watchers {
		fn()
	}
}

// Skipped returns the number of events skipped under a non-stalling
// [Projection.OnError] since the Runner was created.
func (r *Runner) Skipped() int64 { return r.skipped.Load() }

// lease returns the [Lease] this runner uses for seg.
func (r *Runner) lease(seg int) Lease {
	return Lease{Name: r.p.Name, Segment: seg, Owner: r.p.Owner}
}

// readOptions builds the ReadOptions passed to the global reader on each
// read.
func (r *Runner) readOptions() eventsourcing.ReadOptions {
	return eventsourcing.ReadOptions{
		Limit:      r.p.BatchSize,
		EventTypes: r.p.Events,
	}
}
