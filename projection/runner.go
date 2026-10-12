package projection

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/terraskye/eventsourcing"
)

// Default values for [Projection]'s tuning fields.
const (
	DefaultBatchSize    = 500
	DefaultPollInterval = time.Second
	DefaultClaimTTL     = 15 * time.Second
)

var (
	// ErrNotActive is returned by [Runner.Rebuild] when this instance does
	// not currently hold the projection, so no other instance can either
	// (it would do nothing from here).
	ErrNotActive = errors.New("projection: runner is not active on this instance")

	// ErrNoReset is returned by [Runner.Rebuild] when the projection has no
	// Reset function registered.
	ErrNoReset = errors.New("projection: rebuild requires a Reset function")

	// ErrSegmented is returned by [Runner.Rebuild] on a segmented projection.
	// Rebuild a segmented projection by bumping Version and redeploying, or
	// run it under a new name.
	ErrSegmented = errors.New("projection: segmented projections are rebuilt under a new name")
)

// Router routes events to a projector's handlers and knows which event
// types it handles.
//
// [*eventsourcing.EventGroupProcessor] satisfies Router, so the value a
// projector's EventHandlers method returns can be passed to [NewRunner]
// as is.
type Router interface {
	eventsourcing.EventHandler

	// StreamFilter returns the registered names of every event type the
	// router handles. The runner reads only these event types.
	StreamFilter() []string
}

// SegmentKey picks which segment an event belongs to in a segmented
// projection; see [Projection.Segments]. It must return the same key for
// every event that touches the same read-model rows, so that no two
// segments ever write the same row.
type SegmentKey func(env *eventsourcing.Envelope) string

// ByStreamID is a [SegmentKey] that partitions on the envelope's StreamID,
// for read models whose rows are keyed by stream.
func ByStreamID(env *eventsourcing.Envelope) string { return env.StreamID }

// SegmentOf maps key to a segment in [0, segments). It is stable across
// processes and releases, so every instance routes an event to the same
// segment.
func SegmentOf(key string, segments int) int {
	if segments <= 1 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return int(h.Sum32() % uint32(segments))
}

// ErrorPolicy decides what the runner does when a handler returns an error
// for an event.
type ErrorPolicy int

const (
	// StallOnError stops the segment at the failing event: the runner
	// reports [Stalled], backs off and retries until the handler succeeds
	// or the projection is rebuilt. Nothing is skipped silently. Default.
	StallOnError ErrorPolicy = iota

	// SkipWhenLive skips failing events while [Live], and stalls on them
	// while catching up. Keeps a running projection available while a
	// rebuild, which can be verified before going live, stays strict.
	SkipWhenLive

	// SkipAlways skips failing events in every phase.
	SkipAlways
)

// String returns the policy's name.
func (p ErrorPolicy) String() string {
	switch p {
	case StallOnError:
		return "stall_on_error"
	case SkipWhenLive:
		return "skip_when_live"
	case SkipAlways:
		return "skip_always"
	default:
		return "unknown"
	}
}

// Projection is a projection's definition and tuning. The zero value for
// every optional field picks sensible defaults, so a simple projection
// looks like:
//
//	r := projection.NewRunner(projection.Projection{
//		Name:     "task-list",
//		Handlers: projector.EventHandlers(),
//	}, reader, store)
//
// A durable projection adds Version and Reset so it can be rebuilt:
//
//	r := projection.NewRunner(projection.Projection{
//		Name:     "task-list",
//		Handlers: projector.EventHandlers(),
//		Version:  2,
//		Reset:    projector.Reset,
//	}, reader, store)
//
// A projection whose one instance cannot keep up is segmented:
//
//	r := projection.NewRunner(projection.Projection{
//		Name:       "order-summaries-v1",
//		Handlers:   projector.EventHandlers(),
//		Segments:   64,
//		SegmentKey: projection.ByStreamID,
//	}, reader, store)
//
// Nothing else changes. A segmented projection is just the same loop
// scoped to each segment the store lets this instance claim.
type Projection struct {
	// Name identifies the projection. It keys the checkpoint and the
	// claim, so it must be unique per read model and stable across
	// deploys; renaming a projection makes it start from scratch.
	Name string

	// Handlers routes events to the projector's methods. The runner reads
	// only the event types Handlers.StreamFilter reports, each of which
	// must be registered with [eventsourcing.RegisterEvent].
	Handlers Router

	// Version is the version of the projector's code, stored with the
	// checkpoint. When the runner starts a segment and finds a stored
	// version different from this one, the segment is rebuilt before
	// processing new events. Bump the version whenever the projector
	// starts writing the read model differently; see Reset.
	//
	// Default 0: no automatic rebuilds (Rebuild can still be called).
	// Setting a non-zero Version requires Reset.
	Version int

	// Reset wipes the read model so the projection can be rebuilt. It runs
	// in the unit of work that moves the checkpoint back to zero, through
	// the same context handlers use (e.g. postgres.Tx(ctx)):
	//
	//	func (p *Projector) Reset(ctx context.Context) error {
	//		_, err := postgres.Tx(ctx).Exec(ctx, "TRUNCATE tasks")
	//		return err
	//	}
	//
	// Required for Version > 0 and for [Runner.Rebuild].
	Reset func(ctx context.Context) error

	// Segments is how many segments the projection is split into. 0 or 1
	// means a single-segment projection that one instance runs at a time;
	// a value above 1 segments it, with instances sharing the segments
	// through the [Store]'s claim mechanism.
	//
	// Set this only when one instance cannot keep up. The segment count is
	// fixed per name; changing it takes a new name.
	Segments int

	// SegmentKey picks which segment an event belongs to; required when
	// Segments > 1. [ByStreamID] fits read models whose rows are per
	// stream. Pick a key that keeps every read-model row within one
	// segment, or two segments will race on the same row.
	SegmentKey SegmentKey

	// OnError is what the runner does when a handler fails on an event;
	// see [ErrorPolicy]. Default [StallOnError].
	OnError ErrorPolicy

	// Owner identifies this instance to the store, so other instances see
	// who holds a segment. Defaults to "hostname/pid".
	Owner string

	// BatchSize is the maximum number of events read and committed in one
	// unit of work. Default [DefaultBatchSize].
	BatchSize int

	// PollInterval is how often the runner reads again when nothing woke it
	// earlier. Also how often it tries to claim segments it does not own.
	// Default [DefaultPollInterval].
	PollInterval time.Duration

	// ClaimTTL is how long a claim stays valid without a Commit or
	// SaveStatus to refresh it. A crashed instance's segments become
	// available again after this long. Default [DefaultClaimTTL].
	ClaimTTL time.Duration

	// Logger is where the runner logs failures and segment moves. Default
	// [slog.Default].
	Logger *slog.Logger
}

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

	mu       sync.Mutex
	segs     map[int]*segmentState
	watchers []func()

	rebuilds     chan rebuildRequest
	skippedCount int64 // atomic
}

type segmentState struct {
	status  Status
	changed chan struct{} // closed and replaced on every status update, for WaitUntil
}

type rebuildRequest struct {
	ctx  context.Context
	done chan error
}

// NewRunner creates a Runner for p against reader and store.
//
// It panics on wiring mistakes that would otherwise fail silently: an empty
// Name, nil Handlers/reader/store, Version > 0 without Reset, Segments > 1
// without SegmentKey, or Handlers.StreamFilter() returning no event types.
func NewRunner(p Projection, reader eventsourcing.GlobalReader, store Store) *Runner {
	switch {
	case p.Name == "":
		panic("projection: Projection.Name is required")
	case p.Handlers == nil:
		panic(fmt.Sprintf("projection %s: Handlers is required", p.Name))
	case reader == nil:
		panic(fmt.Sprintf("projection %s: reader is required", p.Name))
	case store == nil:
		panic(fmt.Sprintf("projection %s: store is required", p.Name))
	}
	if p.Version != 0 && p.Reset == nil {
		panic(fmt.Sprintf("projection %s: Version > 0 requires Reset", p.Name))
	}
	segments := p.Segments
	if segments < 1 {
		segments = 1
	}
	if segments > 1 && p.SegmentKey == nil {
		panic(fmt.Sprintf("projection %s: Segments > 1 requires SegmentKey", p.Name))
	}
	if filter := p.Handlers.StreamFilter(); len(filter) == 0 {
		panic(fmt.Sprintf("projection %s: handlers cover no registered event types; register them with eventsourcing.RegisterEvent", p.Name))
	}

	// Fill defaults.
	if p.BatchSize <= 0 {
		p.BatchSize = DefaultBatchSize
	}
	if p.PollInterval <= 0 {
		p.PollInterval = DefaultPollInterval
	}
	if p.ClaimTTL <= 0 {
		p.ClaimTTL = DefaultClaimTTL
	}
	if p.Logger == nil {
		p.Logger = slog.Default()
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
		segs:     make(map[int]*segmentState, segments),
		rebuilds: make(chan rebuildRequest),
	}
	for i := 0; i < segments; i++ {
		r.segs[i] = &segmentState{
			status:  Status{Phase: Starting, Version: p.Version, Segment: i},
			changed: make(chan struct{}),
		}
	}
	return r
}

func defaultOwner() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown"
	}
	return host + "/" + strconv.Itoa(os.Getpid())
}

// Name returns the projection's name.
func (r *Runner) Name() string { return r.p.Name }

// Run processes events until ctx is done, then returns ctx.Err().
//
// Run never returns because of a failure. When a batch fails the runner
// reports [Stalled], backs off and retries from its last committed
// checkpoint, until the handler succeeds or the projection is rebuilt.
//
// On start, Run calls [Store.Init] with the configured segment count. For
// each segment, Run competes for the claim: on success it loads the
// checkpoint and processes events; on failure it reports [Standby] for
// that segment and keeps trying. If the stored version differs from
// [Projection.Version] on a segment, that segment is rebuilt first.
//
// Call Run once per Runner.
func (r *Runner) Run(ctx context.Context) error {
	if err := r.store.Init(ctx, r.p.Name, r.segments); err != nil {
		return fmt.Errorf("projection %s: init: %w", r.p.Name, err)
	}

	// Each segment runs its own loop. For an unsegmented projection
	// (Segments <= 1) that is just one loop; for a segmented projection it
	// is N loops competing for N claims, which spread across the instances
	// of this program through the claim mechanism.
	wg := sync.WaitGroup{}
	// A rebuild request goes to the first segment of an unsegmented
	// projection; for segmented ones Rebuild is refused.
	for i := 0; i < r.segments; i++ {
		wg.Add(1)
		go func(seg int) {
			defer wg.Done()
			r.runSegment(ctx, seg)
		}(i)
	}
	wg.Wait()
	return ctx.Err()
}

// Status returns the projection's status as seen by this instance.
//
// For a segmented projection the returned status combines its segments:
// Phase is the furthest-behind phase of any segment (Stalled, then
// Starting/Standby, CatchingUp, Live); Position is the lowest; Head the
// highest; LastError the first non-empty.
func (r *Runner) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.combinedStatus()
}

func (r *Runner) combinedStatus() Status {
	out := Status{Phase: Live, Version: r.p.Version}
	first := true
	for _, s := range r.segs {
		if first {
			out.Position = s.status.Position
			out.Head = s.status.Head
			first = false
		} else {
			if s.status.Position < out.Position {
				out.Position = s.status.Position
			}
			if s.status.Head > out.Head {
				out.Head = s.status.Head
			}
		}
		if phaseRank(s.status.Phase) < phaseRank(out.Phase) {
			out.Phase = s.status.Phase
		}
		if out.LastError == "" && s.status.LastError != "" {
			out.LastError = s.status.LastError
		}
		if out.Owner == "" && s.status.Owner != "" {
			out.Owner = s.status.Owner
		}
	}
	return out
}

// phaseRank orders phases from worst to best, for combining segment
// statuses: a projection is only Live when every segment is Live.
func phaseRank(p Phase) int {
	switch p {
	case Stalled:
		return 0
	case Starting, Standby:
		return 1
	case CatchingUp:
		return 2
	case Live:
		return 3
	}
	return 1
}

// Live reports whether the projection is live: caught up with the head of
// the log on every segment, on whichever instance owns each segment.
//
// On segments this instance owns, Live answers from memory. On segments
// another instance owns, it reads [Store.LoadStatuses], and treats any
// segment it cannot read as not live.
//
// Live implements [Dependency].
func (r *Runner) Live(ctx context.Context) (bool, error) {
	if r.segments == 1 {
		r.mu.Lock()
		p := r.segs[0].status.Phase
		owner := r.segs[0].status.Owner
		r.mu.Unlock()
		if owner == r.p.Owner {
			return p == Live, nil
		}
		// Standby: read from store.
	}
	statuses, err := r.store.LoadStatuses(ctx, r.p.Name)
	if err != nil {
		return false, err
	}
	if len(statuses) < r.segments {
		return false, nil
	}
	for _, s := range statuses {
		if s.Phase != Live {
			return false, nil
		}
	}
	return true, nil
}

// WaitUntil blocks until the projection has processed the log up to and
// including position on every segment, or until ctx is done.
//
// position is an [eventsourcing.Envelope.GlobalVersion], typically from the
// [eventsourcing.AppendResult] of the command whose effects the caller
// wants to see.
//
// Events the projection does not handle count as processed. On segments
// this instance owns, WaitUntil watches memory; on segments another
// instance owns, it polls [Store.LoadStatuses] at [Projection.PollInterval].
func (r *Runner) WaitUntil(ctx context.Context, position uint64) error {
	for {
		if ok, err := r.reached(ctx, position); err != nil {
			return err
		} else if ok {
			return nil
		}

		// Wait for a change: either a local commit, or at latest the next
		// poll if we are watching another instance.
		r.mu.Lock()
		localChans := make([]<-chan struct{}, 0, r.segments)
		for _, s := range r.segs {
			if s.status.Owner == r.p.Owner {
				localChans = append(localChans, s.changed)
			}
		}
		r.mu.Unlock()

		if len(localChans) == r.segments {
			// All segments are local; wait for any of them to change.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-waitAny(localChans):
			}
			continue
		}

		// At least one segment is remote; poll.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.p.PollInterval):
		}
	}
}

func (r *Runner) reached(ctx context.Context, position uint64) (bool, error) {
	// Local segments are consulted from memory.
	r.mu.Lock()
	allLocal := true
	for _, s := range r.segs {
		if s.status.Owner != r.p.Owner {
			allLocal = false
			continue
		}
		if s.status.Position < position {
			r.mu.Unlock()
			return false, nil
		}
	}
	r.mu.Unlock()
	if allLocal {
		return true, nil
	}
	statuses, err := r.store.LoadStatuses(ctx, r.p.Name)
	if err != nil {
		return false, err
	}
	if len(statuses) < r.segments {
		return false, nil
	}
	for _, s := range statuses {
		if s.Position < position {
			return false, nil
		}
	}
	return true, nil
}

// waitAny closes receives from any of the channels. Used in WaitUntil.
func waitAny(chans []<-chan struct{}) <-chan struct{} {
	if len(chans) == 1 {
		return chans[0]
	}
	out := make(chan struct{})
	var once sync.Once
	for _, c := range chans {
		go func(c <-chan struct{}) {
			<-c
			once.Do(func() { close(out) })
		}(c)
	}
	return out
}

// Rebuild wipes the read model and processes the log again from the start.
//
// Rebuild only reaches the runner in this process. It returns
// [ErrNotActive] when this instance does not currently hold the segment,
// [ErrNoReset] when the projection has no Reset, and [ErrSegmented] on a
// segmented projection. To rebuild across instances or a segmented
// projection, bump [Projection.Version].
func (r *Runner) Rebuild(ctx context.Context) error {
	if r.segments > 1 {
		return ErrSegmented
	}
	if r.p.Reset == nil {
		return ErrNoReset
	}
	req := rebuildRequest{ctx: ctx, done: make(chan error, 1)}
	select {
	case r.rebuilds <- req:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-req.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Nudge asks the runner to read now instead of waiting for its next poll.
//
// Nudge never blocks, and nudges coalesce. Only reaches this process's
// runner; other instances rely on [eventsourcing.Notifier] or on polling.
func (r *Runner) Nudge() {
	select {
	case r.nudge <- struct{}{}:
	default:
	}
}

// Watch registers fn to be called after every committed batch that
// contained events, and on every change of [Phase]. It is how an
// automation waiting on this projection learns of new work without polling.
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
	watchers := make([]func(), len(r.watchers))
	copy(watchers, r.watchers)
	r.mu.Unlock()
	for _, fn := range watchers {
		fn()
	}
}

// Dependency reports whether a projection is live, wherever it runs.
// Automations take Dependencies, so they can wait for the read model they
// work from without knowing where it runs.
//
// [*Runner] implements it, and so does [Remote] for a projection run by
// another program.
type Dependency interface {
	Name() string
	Live(ctx context.Context) (bool, error)
}

// Watcher is optionally implemented by a [Dependency] that can call back
// when its projection changes.
type Watcher interface {
	Watch(fn func())
}

// Remote returns a [Dependency] for the projection called name, run by a
// different program, judged by the status its runner publishes to store.
//
// Use it in an automation whose todo-list projection lives in another
// service: the dependency pauses the automation until that projection has
// caught up.
func Remote(name string, store Store) Dependency {
	return &remote{name: name, store: store}
}

type remote struct {
	name  string
	store Store
}

func (r *remote) Name() string { return r.name }

func (r *remote) Live(ctx context.Context) (bool, error) {
	statuses, err := r.store.LoadStatuses(ctx, r.name)
	if err != nil || len(statuses) == 0 {
		return false, err
	}
	for _, s := range statuses {
		if s.Phase != Live {
			return false, nil
		}
	}
	return true, nil
}

var (
	_ Dependency = (*Runner)(nil)
	_ Watcher    = (*Runner)(nil)
	_ Nudger     = (*Runner)(nil)
)

// readOptions builds the ReadOptions passed to the global reader on each
// read.
func (r *Runner) readOptions() eventsourcing.ReadOptions {
	return eventsourcing.ReadOptions{
		Limit:      r.p.BatchSize,
		EventTypes: r.p.Handlers.StreamFilter(),
	}
}
