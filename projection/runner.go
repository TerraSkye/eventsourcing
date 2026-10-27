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
	"time"

	"github.com/terraskye/eventsourcing"
)

var (
	// ErrUnknownProjection is returned by [Store.RequestRebuild] and
	// [DeadLetterStore.RequestReplay] for a projection no runner has
	// declared: there is nothing to rebuild or replay yet.
	ErrUnknownProjection = errors.New("projection: no runner has declared this projection")

	// ErrNoReset is returned by [Runner.Rebuild] when the projection has no
	// Reset function registered.
	ErrNoReset = errors.New("projection: rebuild requires a Reset function")

	// ErrEventTypesChanged stalls a segment whose stored checkpoint was
	// written for a different [Definition.EventTypes] than the runner's,
	// when the runner cannot rebuild: the projection has no Reset, or it is
	// segmented, where old and new instances of a rollout would otherwise
	// reset each other. Carrying on would never apply the history of an
	// added type. Bump [Definition.Version] with the change, or run the
	// projection under a new Name.
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

	// probe reads the status other instances publish for their segments.
	probe *Probe

	// fingerprint identifies p.EventTypes; see Checkpoint.Fingerprint.
	fingerprint string

	mu       sync.Mutex
	segs     []*segmentState // indexed by segment
	changed  chan struct{}   // closed and replaced on every status change, for WaitUntil
	watchers []func()

	// Replay requests from the store; see replay.go. pendingReplays holds
	// those queued for each segment, taken between batches; queuedReplays
	// every request queued and not yet finished or dropped, so a poll does
	// not queue it twice. checkReplays asks the poll to run at once, and
	// replayed is closed and replaced whenever a segment here finishes one.
	// While replayWaiters ReplayStream calls wait here, replayErrs keeps the
	// errors segments here failed each request with, so those calls return
	// them as they were rather than as their text.
	// pollReplays asks the next regular poll to read the store even though
	// no segment here holds dead letters; see [Runner.replaysWanted].
	pendingReplays map[int][]Replay
	queuedReplays  map[Replay]bool
	checkReplays   chan struct{}
	replayed       chan struct{}
	replayWaiters  int
	replayErrs     map[uint64][]error
	pollReplays    bool

	// checkRebuild asks segment 0 to look for a rebuild request in the
	// store at once, rather than at its next regular check.
	checkRebuild bool
	// warnedRebuild is the last rebuild request logged as impossible for
	// want of a Reset. Only segment 0's goroutine uses it.
	warnedRebuild uint64
	// rebuilt is closed and replaced after every rebuild segment 0 tries
	// on this instance, for Rebuild to look at the store again.
	rebuilt chan struct{}
	// resetting reports that a rebuild on this instance is under way: its
	// Store.Reset may have committed before the runner caught up in memory.
	resetting bool
	// rebuildFailed is the last rebuild request a rebuild on this
	// instance failed to serve, and rebuildErr why.
	rebuildFailed uint64
	rebuildErr    error

	// Membership, from the last Store.Heartbeat; see instances.go. quota is
	// how many segments this instance should hold, held how many it holds
	// or is claiming, instancesOK whether that heartbeat succeeded, and
	// stable how many heartbeats in a row saw the same membership.
	quota       int
	held        int
	instancesOK bool
	stable      int
	membership  string

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

	// reserved reports that the segment counts against this instance's
	// quota: it holds the claim or is claiming it; see [Runner.reserve].
	reserved bool

	// parked holds the streams with a dead letter in this segment, whose
	// later events are skipped. Only the segment's own goroutine uses it.
	parked map[string]bool

	// hasParked reports that parked is not empty, for the replay poll,
	// which only reads the store while a segment here holds dead letters;
	// see [Runner.noteParked].
	hasParked bool

	// rebuilding reports that the segment was rebuilt and has not reached
	// the head of the log since. Only the segment's own goroutine uses it.
	rebuilding bool

	// nudge wakes the segment at the head of the log; see [Runner.Nudge].
	nudge chan struct{}
}

// NewRunner creates a Runner for p against reader and store.
//
// It panics on wiring mistakes that would otherwise fail silently: an empty
// Name, nil Handler/reader/store, no EventTypes or an unregistered one,
// Version > 0 without Reset, Segments > 1 without SegmentKey, or a
// PollInterval no shorter than ClaimTTL.
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
	if p.PollInterval >= p.ClaimTTL {
		// An idle segment refreshes its claim once per PollInterval, so
		// the claim would expire between two polls and move on every one.
		panic(fmt.Sprintf("projection %q: PollInterval (%s) must be shorter than ClaimTTL (%s)", p.Name, p.PollInterval, p.ClaimTTL))
	}
	p.Owner = ownerOf(p.Owner)
	if p.Logger == nil {
		p.Logger = slog.Default()
	}

	r := &Runner{
		p:        p,
		reader:   reader,
		store:    store,
		segments: segments,

		fingerprint: fingerprint(p.EventTypes),
		segs:        make([]*segmentState, segments),

		pendingReplays: map[int][]Replay{},
		queuedReplays:  map[Replay]bool{},
		replayErrs:     map[uint64][]error{},
		checkReplays:   make(chan struct{}, 1),
		replayed:       make(chan struct{}),
		changed:        make(chan struct{}),
		rebuilt:        make(chan struct{}),
	}
	r.probe = NewProbe(ProbeDefinition{
		Name:         p.Name,
		PollInterval: p.PollInterval,
		ClaimTTL:     p.ClaimTTL,
	}, store)
	r.probe.segments = segments
	for i := range r.segs {
		r.segs[i] = &segmentState{
			status: Status{Phase: Starting, Version: p.Version, Segment: i},
			lease:  Lease{Name: p.Name, Segment: i, Owner: p.Owner},
			parked: map[string]bool{},
			nudge:  make(chan struct{}, 1),
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

// ownerOf returns the Owner a runner configured with owner runs under:
// owner, or "hostname/pid" when it is empty (with "unknown" for a hostname
// that cannot be read), followed by a random part. The random part makes
// every Runner its own instance in [Store.Heartbeat], even when several
// share a configured Owner, and tells apart, in LoadStatuses, two runners
// in one process and a restarted process from the one before it, which in
// a container usually had the same pid.
func ownerOf(owner string) string {
	if owner == "" {
		host, err := os.Hostname()
		if err != nil || host == "" {
			host = "unknown"
		}
		owner = host + "/" + strconv.Itoa(os.Getpid())
	}
	var b [4]byte
	_, _ = rand.Read(b[:]) // never returns an error
	return owner + "/" + hex.EncodeToString(b[:])
}

// Name returns the projection's name.
func (r *Runner) Name() string { return r.p.Name }

// Owner returns the Owner this runner claims segments and sends heartbeats
// under: [Definition.Owner] with a random part appended; see
// [Definition.Owner].
func (r *Runner) Owner() string { return r.p.Owner }

// CanRebuild reports whether the projection has a [Definition.Reset], which
// [Runner.Rebuild] needs to wipe the read model.
func (r *Runner) CanRebuild() bool { return r.p.Reset != nil }

// Run processes events until ctx is done, then returns ctx.Err(). It
// returns early only when it cannot start: when reading the head of the log,
// [Store.Declare] or the first [Store.Heartbeat] keeps failing for
// [Definition.ClaimTTL], with the last error, or at once when Declare
// returns [ErrSegmentsChanged].
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
// The instances of a projection spread its segments evenly: each one
// records itself with [Store.Heartbeat], claims segments only up to its
// share, and releases segments above it between two batches, so with S
// segments and N instances each holds S/N, rounded down or up. A single
// instance holds every segment.
//
// Call Run once per Runner. On return it has released every claim it held
// and removed itself from the live instances.
func (r *Runner) Run(ctx context.Context) error {
	if err := r.start(ctx); err != nil {
		return err
	}
	var background sync.WaitGroup
	background.Add(2)
	go func() {
		defer background.Done()
		r.runMembership(ctx)
	}()
	go func() {
		defer background.Done()
		r.runReplays(ctx)
	}()

	// One loop per segment. The loops compete for the claims with the
	// loops of every other instance of this program, each claiming only
	// up to the instance's quota.
	var wg sync.WaitGroup
	wg.Add(r.segments)
	for seg := range r.segments {
		go func() {
			defer wg.Done()
			r.runSegment(ctx, seg)
		}()
	}
	wg.Wait()
	background.Wait()
	r.leave(ctx)
	return ctx.Err()
}

// start reads the head of the log, declares the segments and sends the
// first heartbeat, trying all three again with the stall backoff for up to
// ClaimTTL. A blip at startup then does not end Run, while a store that
// keeps failing, such as one missing a table, still does. A segment count
// that differs from the stored one is not retried.
func (r *Runner) start(ctx context.Context) error {
	retry := newRetryBackOff()
	giveUp := time.Now().Add(r.p.ClaimTTL)
	for {
		err := r.startOnce(ctx)
		if err == nil || ctx.Err() != nil || errors.Is(err, ErrSegmentsChanged) {
			return err
		}
		wait := retry.NextBackOff()
		if time.Now().Add(wait).After(giveUp) {
			return err
		}
		r.p.Logger.WarnContext(ctx, "projection start failed; retrying",
			slog.String("projection", r.p.Name), slog.String("owner", r.p.Owner),
			slog.String("error", err.Error()), slog.Duration("retry_in", wait))
		sleepCtx(ctx, wait)
	}
}

// startOnce makes one attempt of [Runner.start].
func (r *Runner) startOnce(ctx context.Context) error {
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
	// The first heartbeat sets the quota before any segment is claimed, so
	// a single instance takes every segment at once and a later one does
	// not take more than its share.
	if err := r.heartbeat(ctx); err != nil {
		return fmt.Errorf("projection %q: heartbeat: %w", r.p.Name, err)
	}
	return nil
}

// Rebuild wipes the read model and processes the log again from the start,
// wherever the projection runs. It records a request in the [Store], which
// the runner holding segment 0 picks up between two batches: at once when
// that is this instance, otherwise within its [Definition.PollInterval].
// Rebuild then waits until the reset has run. A segmented projection is
// rebuilt as a whole.
//
// The request is kept in the store. When ctx ends first, Rebuild returns
// its error but the rebuild still runs, even if instances restart or
// claims move in between. Requests made before a rebuild runs are all
// served by it. Any process with the store can request one the same way
// with [Store.RequestRebuild], without running the projection.
//
// When the reset fails, the request stays pending and segment 0 stalls and
// tries again after its backoff, as [Status] shows with its LastError.
// Rebuild returns that error: as it was when the reset failed on this
// instance, and as its text, recorded with [Store.FailRebuild], when it
// failed on another.
//
// Rebuild returns [ErrNoReset] when the projection has no Reset, and an
// error wrapping [ErrUnknownProjection] before any runner declared it.
func (r *Runner) Rebuild(ctx context.Context) error {
	if r.p.Reset == nil {
		return ErrNoReset
	}
	n, err := r.store.RequestRebuild(ctx, r.p.Name)
	if err != nil {
		return fmt.Errorf("projection %q: request rebuild: %w", r.p.Name, err)
	}
	r.mu.Lock()
	r.checkRebuild = true
	r.mu.Unlock()
	r.Nudge()

	tick := time.NewTicker(r.p.PollInterval)
	defer tick.Stop()
	for {
		// A failure is recorded, in the store and here, after the Reset of
		// the rebuild that failed, so reading it before done means a done
		// read afterwards covers any Reset that served request n even so.
		r.mu.Lock()
		rebuilt, localFailed, localErr := r.rebuilt, r.rebuildFailed, r.rebuildErr
		r.mu.Unlock()
		failed, failMsg, err := r.store.RebuildFailure(ctx, r.p.Name)
		if err != nil {
			return fmt.Errorf("projection %q: read rebuild failure: %w", r.p.Name, err)
		}
		_, done, err := r.store.Rebuilds(ctx, r.p.Name)
		if err != nil {
			return fmt.Errorf("projection %q: read rebuild requests: %w", r.p.Name, err)
		}
		r.mu.Lock()
		resetting := r.resetting
		r.mu.Unlock()
		switch {
		case done >= n && !resetting:
			return nil
		case done >= n:
			// Served by the rebuild under way here, whose Reset has
			// committed: wait until the runner has caught up with it, so
			// a WaitUntil after Rebuild does not see positions from
			// before the wipe.
		case localFailed >= n:
			return localErr
		case failed >= n:
			return errors.New(failMsg)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-rebuilt: // a rebuild on this instance served or failed it
		case <-tick.C: // a rebuild elsewhere only shows in the store
		}
	}
}

// rebuildTried records that a rebuild on this instance is over, having
// tried to serve rebuild request requested (0 for one a Version change
// started), failing with err, and wakes the Rebuild calls waiting on this
// instance. A rebuild cut short by the claim moving or Run stopping has not
// failed: whichever runner holds segment 0 next serves the request.
//
// A failure is also recorded with [Store.FailRebuild], for Rebuild calls
// on other instances; when that fails too, they keep waiting, and the
// failure is logged.
func (r *Runner) rebuildTried(ctx context.Context, requested uint64, err error) {
	failed := requested > 0 && err != nil && ctx.Err() == nil && !errors.Is(err, ErrClaimLost)
	if failed {
		if ferr := r.store.FailRebuild(ctx, r.lease(0), requested, err.Error()); ferr != nil {
			r.log(ctx, slog.LevelWarn, 0, r.position(0),
				"projection rebuild failure not recorded; Rebuild calls on other instances keep waiting",
				slog.Uint64("request", requested), slog.String("error", ferr.Error()))
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resetting = false
	if failed {
		r.rebuildFailed, r.rebuildErr = requested, err
	}
	close(r.rebuilt)
	r.rebuilt = make(chan struct{})
}

// rebuildDue returns the last rebuild request when segment 0 should rebuild
// now, as it was requested and Reset has not served it, and 0 otherwise.
// It asks the store at most once per PollInterval, and at once after a
// Rebuild on this instance. last is when the segment last asked.
func (r *Runner) rebuildDue(ctx context.Context, last *time.Time) (uint64, error) {
	r.mu.Lock()
	asked := r.checkRebuild
	r.checkRebuild = false
	r.mu.Unlock()
	if !asked && !last.IsZero() && time.Since(*last) < r.p.PollInterval {
		return 0, nil
	}
	*last = time.Now()

	requested, done, err := r.store.Rebuilds(ctx, r.p.Name)
	if err != nil {
		return 0, fmt.Errorf("projection %q: read rebuild requests: %w", r.p.Name, err)
	}
	if requested <= done {
		return 0, nil
	}
	if r.p.Reset == nil {
		// Another program asked through the store, not knowing there is
		// nothing to wipe with. Leave the request pending, visible in the
		// status, and say why once.
		if r.warnedRebuild != requested {
			r.log(ctx, slog.LevelWarn, 0, r.position(0),
				"projection rebuild requested, but the projection has no Reset; it stays pending",
				slog.Uint64("request", requested))
			r.warnedRebuild = requested
		}
		return 0, nil
	}
	return requested, nil
}

// Nudge asks the runner to read now instead of waiting for its next poll,
// on every segment it processes.
//
// Nudge never blocks, and nudges coalesce. Only reaches this process's
// runner; other instances rely on polling.
func (r *Runner) Nudge() {
	for _, s := range r.segs { // fixed after NewRunner
		select {
		case s.nudge <- struct{}{}:
		default: // a nudge is already pending
		}
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
