package projection

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/terraskye/eventsourcing"
)

var (
	// ErrNotActive is returned by [Runner.Rebuild] when this instance is not
	// working the projection: Run has not started or has returned, or
	// another instance holds the claim. A rebuild only reaches the runner
	// in this process; bump [Definition.Version] to rebuild wherever the
	// projection runs.
	ErrNotActive = errors.New("projection: runner is not active on this instance")

	// ErrNoReset is returned by [Runner.Rebuild] when the projection has no
	// Reset function registered.
	ErrNoReset = errors.New("projection: rebuild requires a Reset function")

	// ErrRebuildSegmented is returned by [Runner.Rebuild] on a segmented
	// projection. A segmented projection is rebuilt by running it under a
	// new name; see [Definition.Version].
	ErrRebuildSegmented = errors.New("projection: segmented projections are rebuilt under a new name")

	// ErrEventTypesChanged stalls a segment whose stored checkpoint was
	// written for a different [Definition.EventTypes] than the runner's,
	// when the runner cannot rebuild: the projection has no Reset, or it is
	// segmented. Carrying on would never apply the history of an added
	// type. Bump [Definition.Version], or run the projection under a new
	// Name.
	ErrEventTypesChanged = errors.New("projection: EventTypes changed since the checkpoint was written")

	// ErrStreamParked is the cause recorded for an event that was skipped
	// without running the handler, because an earlier event of its stream
	// was skipped. Applying it would show the read model a later state of
	// the stream without the earlier one; see [Runner.ReplayStream].
	ErrStreamParked = errors.New("projection: an earlier event of the stream was skipped")

	// ErrNoDeadLetterStore is returned by [Runner.ReplayStream] when the
	// Store does not implement [DeadLetterStore], so there are no skipped
	// events to replay.
	ErrNoDeadLetterStore = errors.New("projection: the store does not record dead letters")
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
	p        Definition
	reader   eventsourcing.GlobalReader
	store    Store
	segments int // normalized: at least 1

	// fingerprint identifies p.EventTypes; see Checkpoint.Fingerprint.
	fingerprint string

	nudge chan struct{}

	mu             sync.Mutex
	segs           []*segmentState // indexed by segment
	changed        chan struct{}   // closed and replaced on every status change, for WaitUntil
	watchers       []func()
	pendingRebuild *rebuildRequest          // taken by segment 0 between batches
	pendingReplays map[int][]*replayRequest // by segment, taken between batches

	skipped atomic.Int64
}

type segmentState struct {
	status Status

	// lease is this instance's claim on the segment: the Lease Store.Claim
	// last granted, or one with Epoch 0 before the first claim.
	lease Lease

	// active reports that this instance holds the segment's claim and is
	// processing it.
	active bool

	// parked holds the streams with a dead letter in this segment, whose
	// later events are skipped. Only the segment's own goroutine uses it.
	parked map[string]bool

	// rebuilding reports that the segment was rebuilt and has not reached
	// the head of the log since. Only the segment's own goroutine uses it.
	rebuilding bool
}

// replayRequest is a pending [Runner.ReplayStream] for one segment.
type replayRequest struct {
	streamID string
	done     chan struct{}
	err      error
}

func (req *replayRequest) finish(err error) {
	req.err = err
	close(req.done)
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
// Name, nil Handler/reader/store, no EventTypes or an unregistered one,
// Version > 0 without Reset, or Segments > 1 without SegmentKey or with a
// Version.
func NewRunner(p Definition, reader eventsourcing.GlobalReader, store Store) *Runner {
	switch {
	case p.Name == "":
		panic("projection: Definition.Name is required")
	case p.Handler == nil:
		panic(fmt.Sprintf("projection %q: Handler is required", p.Name))
	case len(p.EventTypes) == 0:
		panic(fmt.Sprintf("projection %q: EventTypes is required", p.Name))
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
	for _, name := range p.EventTypes {
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
	if p.Logger == nil {
		p.Logger = slog.Default()
	}

	r := &Runner{
		p:        p,
		reader:   reader,
		store:    store,
		segments: segments,
		nudge:    make(chan struct{}, 1),

		fingerprint: fingerprint(p.EventTypes),
		segs:        make([]*segmentState, segments),

		pendingReplays: map[int][]*replayRequest{},
		changed:        make(chan struct{}),
	}
	for i := range r.segs {
		r.segs[i] = &segmentState{
			status: Status{Phase: Starting, Version: p.Version, Segment: i},
			lease:  Lease{Name: p.Name, Segment: i, Owner: p.Owner},
			parked: map[string]bool{},
		}
	}
	return r
}

// fingerprint returns a short hash of the event type names, independent of
// their order and of duplicates.
func fingerprint(eventTypes []string) string {
	names := slices.Clone(eventTypes)
	slices.Sort(names)
	names = slices.Compact(names)
	sum := sha256.Sum256([]byte(strings.Join(names, "\n")))
	return hex.EncodeToString(sum[:16])
}

// defaultOwner returns "hostname/pid/random", with "unknown" for a hostname
// that cannot be read. The random part tells apart, in LoadStatuses, two
// runners in one process and a restarted process from the one before it,
// which in a container usually had the same pid.
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
// or [Store.Declare] fails, with that error.
//
// Failures while processing never make Run return. When a batch fails the
// runner reports [Stalled], backs off and retries from its last committed
// checkpoint, until the handler succeeds or the projection is rebuilt.
//
// Each segment competes for its claim: on success Run loads the checkpoint
// and processes events; otherwise it reports [Standby] for that segment and
// keeps trying. If the stored version differs from [Definition.Version] on
// a segment, that segment is rebuilt first.
//
// Call Run once per Runner. On return it has released every claim it held.
func (r *Runner) Run(ctx context.Context) error {
	// Events already in the log when the projection is first created count
	// as replayed; see IsReplaying. Declare only uses head for segments it
	// creates, so a restart keeps the boundary of the first start.
	head, err := r.reader.Head(ctx)
	if err != nil {
		return fmt.Errorf("projection %q: read head: %w", r.p.Name, err)
	}
	if err := r.store.Declare(ctx, r.p.Name, r.segments, head); err != nil {
		return fmt.Errorf("projection %q: declare segments: %w", r.p.Name, err)
	}

	// One loop per segment. The loops compete for the claims with the
	// loops of every other instance of this program.
	var wg sync.WaitGroup
	wg.Add(r.segments)
	for seg := range r.segments {
		go func() {
			defer wg.Done()
			r.runSegment(ctx, seg)
		}()
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
// Rebuild returns [ErrRebuildSegmented] on a segmented projection, [ErrNoReset]
// when the projection has no Reset, and [ErrNotActive] when this instance
// is not processing the projection, or stops before the rebuild ran. To
// rebuild wherever the projection runs, bump [Definition.Version]; to
// rebuild a segmented projection, run it under a new Name.
func (r *Runner) Rebuild(ctx context.Context) error {
	if r.segments > 1 {
		return ErrRebuildSegmented
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
// being active, a pending rebuild or replay for it fails with
// [ErrNotActive].
func (r *Runner) setActive(seg int, active bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.segs[seg].active = active
	if active {
		return
	}
	if seg == 0 && r.pendingRebuild != nil {
		r.pendingRebuild.finish(ErrNotActive)
		r.pendingRebuild = nil
	}
	for _, req := range r.pendingReplays[seg] {
		req.finish(ErrNotActive)
	}
	delete(r.pendingReplays, seg)
}

// ReplayStream applies the skipped events of streamID again, in order, on
// every segment this instance is processing, and un-parks the stream once
// they all apply. Call it after fixing whatever made the handler fail. Each
// event is applied and its dead letter removed in one Commit; if the
// handler fails again, ReplayStream returns that error and the stream stays
// parked, with the events not yet applied still recorded.
//
// The runner performs it between two batches. ctx only bounds how long
// ReplayStream waits. It returns [ErrNoDeadLetterStore] when the Store
// keeps no dead letters, and [ErrNotActive] when this instance processes
// no segment of the projection. A stream with nothing to replay on a
// segment is not an error.
func (r *Runner) ReplayStream(ctx context.Context, streamID string) error {
	if _, ok := r.store.(DeadLetterStore); !ok {
		return ErrNoDeadLetterStore
	}

	r.mu.Lock()
	var reqs []*replayRequest
	for seg, st := range r.segs {
		if !st.active {
			continue
		}
		req := &replayRequest{streamID: streamID, done: make(chan struct{})}
		r.pendingReplays[seg] = append(r.pendingReplays[seg], req)
		reqs = append(reqs, req)
	}
	r.mu.Unlock()
	if len(reqs) == 0 {
		return ErrNotActive
	}
	r.Nudge()

	var errs []error
	for _, req := range reqs {
		select {
		case <-req.done:
			if req.err != nil {
				errs = append(errs, req.err)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return errors.Join(errs...)
}

// takeReplays returns the pending replay requests for seg and clears them.
func (r *Runner) takeReplays(seg int) []*replayRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	reqs := r.pendingReplays[seg]
	delete(r.pendingReplays, seg)
	return reqs
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
// [Definition.OnError] since the Runner was created.
func (r *Runner) Skipped() int64 { return r.skipped.Load() }

// lease returns this runner's [Lease] on seg.
func (r *Runner) lease(seg int) Lease {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.segs[seg].lease
}

// claim calls [Store.Claim] for seg under this runner's lease and, when
// granted, keeps the Lease it returns for every later call on seg.
func (r *Runner) claim(ctx context.Context, seg int) (Checkpoint, bool, error) {
	l, cp, ok, err := r.store.Claim(ctx, r.lease(seg), r.p.ClaimTTL)
	if err != nil || !ok {
		return Checkpoint{}, false, err
	}
	r.mu.Lock()
	r.segs[seg].lease = l
	r.mu.Unlock()
	return cp, true, nil
}

// readOptions builds the ReadOptions passed to the global reader on each
// read.
func (r *Runner) readOptions() eventsourcing.ReadOptions {
	return eventsourcing.ReadOptions{
		Limit:      r.p.BatchSize,
		EventTypes: r.p.EventTypes,
	}
}
