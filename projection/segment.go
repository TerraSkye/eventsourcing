package projection

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/terraskye/eventsourcing"
)

// releaseTimeout bounds the Release a segment loop makes on its way out.
const releaseTimeout = 2 * time.Second

// runSegment is the loop for one segment of the projection. A
// single-segment projection runs it once, a segmented projection once per
// segment. It returns when ctx is done.
//
// The loop:
//
//  1. tries to claim the segment from the store;
//  2. if claimed, processes events in batches, each committed through
//     Store.Commit so read-model writes and checkpoint moves land together;
//  3. if not claimed, reports Standby and polls until the claim becomes
//     available;
//  4. on ErrClaimLost, drops the segment and goes back to step 1; on any
//     other error, reports Stalled and backs off before going back;
//  5. on ctx.Done, releases the claim so another instance can take over
//     at once rather than waiting the claim TTL out.
func (r *Runner) runSegment(ctx context.Context, seg int) {
	held := false     // this instance holds the segment's claim
	outdated := false // the stored Version is newer than this runner's
	defer func() {
		// ctx is done by now; release under a context that is not.
		relCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
		defer cancel()
		// Best effort: if Release fails, the claim expires after ClaimTTL
		// and another instance takes over then.
		_ = r.store.Release(relCtx, r.lease(seg))
		if held {
			r.log(relCtx, slog.LevelInfo, seg, r.position(seg), "projection segment released")
		}
	}()

	retry := newRetryBackOff()
	for ctx.Err() == nil {
		cp, ok, err := r.claim(ctx, seg)
		if err != nil {
			// The claim failed, so this instance does not know it holds
			// the segment: leave the owner to the store.
			r.setPhase(seg, Stalled, err.Error(), false)
			wait := retry.NextBackOff()
			r.logStall(ctx, seg, err, wait)
			sleepCtx(ctx, wait)
			continue
		}
		if !ok {
			if held {
				r.log(ctx, slog.LevelWarn, seg, r.position(seg), "projection segment claim lost")
				held = false
			}
			retry.Reset()
			r.setPhase(seg, Standby, "", false)
			sleepCtx(ctx, r.p.PollInterval)
			continue
		}
		if cp.Version > r.p.Version {
			// A newer deployment rebuilt the projection for a later
			// Version. Rebuilding it back would have old and new instances
			// reset each other until the rollout is done, so step aside.
			_ = r.store.Release(ctx, r.lease(seg))
			if !outdated {
				r.log(ctx, slog.LevelWarn, seg, cp.Position, "projection segment left to a newer version",
					slog.Int("version", r.p.Version), slog.Int("stored_version", cp.Version))
				outdated = true
			}
			held = false
			r.setPhase(seg, Standby, "", false)
			sleepCtx(ctx, r.p.PollInterval)
			continue
		}
		outdated = false
		if !held {
			r.log(ctx, slog.LevelInfo, seg, cp.Position, "projection segment claimed")
			held = true
		}

		before := r.position(seg)
		r.setActive(seg, true)
		err = r.work(ctx, seg, cp)
		r.setActive(seg, false)
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, ErrClaimLost):
			// Another instance took the segment; compete for it again.
			r.log(ctx, slog.LevelWarn, seg, r.position(seg), "projection segment claim lost")
			held = false
			retry.Reset()
		default:
			// A stalled segment keeps its claim, so the next Claim
			// succeeds: only a segment that moved before failing starts
			// its backoff over.
			if r.position(seg) != before {
				retry.Reset()
			}
			r.setPhase(seg, Stalled, err.Error(), true)
			r.publishStatus(ctx, seg)
			wait := retry.NextBackOff()
			r.logStall(ctx, seg, err, wait)
			sleepCtx(ctx, wait)
		}
	}
}

// work holds the claim on seg and processes events from cp. It returns
// when the claim is lost, ctx is done, or a batch fails; it never returns
// nil.
func (r *Runner) work(ctx context.Context, seg int, cp Checkpoint) error {
	if err := r.loadParked(ctx, seg, cp); err != nil {
		return err
	}

	// Rebuild if the configured Version is newer than the stored one, or
	// EventTypes changed, and there is already something to wipe. A fresh
	// segment (nothing processed yet) just picks both up on its first
	// commit, and an empty stored fingerprint is not a change. A stored
	// Version newer than the configured one never gets here: runSegment
	// steps aside instead. NewRunner requires a Reset for any Version.
	//
	// An EventTypes change alone only rebuilds an unsegmented projection.
	// Two lists cannot be ordered, so during a rollout old and new
	// instances of a segmented one would reset each other; bumping Version
	// with the change gives the order.
	if cp.Position > 0 {
		upgraded := cp.Version < r.p.Version
		typesChanged := cp.Fingerprint != "" && cp.Fingerprint != r.fingerprint
		switch {
		case upgraded, typesChanged && r.p.Reset != nil && r.segments == 1:
			var err error
			if cp, err = r.rebuild(ctx, seg); err != nil {
				return err
			}
		case typesChanged:
			return fmt.Errorf("projection %q segment %d: %w", r.p.Name, seg, ErrEventTypesChanged)
		}
	}

	var rebuildChecked time.Time
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		// Segment 0 serves rebuild requests, from this instance or any
		// other, between two batches.
		if seg == 0 {
			requested, err := r.rebuildDue(ctx, &rebuildChecked)
			if err != nil {
				return err
			}
			if requested > 0 {
				cp, err = r.rebuild(ctx, seg)
				r.rebuildTried(ctx, requested, err)
				if err != nil {
					return err
				}
			}
		}
		for _, req := range r.takeReplays(seg) {
			err := r.replayStream(ctx, seg, cp, req.streamID)
			req.finish(err)
			if errors.Is(err, ErrClaimLost) {
				return err
			}
		}

		res, err := r.reader.ReadAll(ctx, cp.Position, r.readOptions())
		if err != nil {
			return fmt.Errorf("projection %q: read after %d: %w", r.p.Name, cp.Position, err)
		}

		// At the end of the log the read scanned up to the head; otherwise
		// ask for it, so Status.Head shows how far behind the segment is.
		next := max(res.Next, cp.Position)
		head := next
		if !res.AtEnd {
			if head, err = r.reader.Head(ctx); err != nil {
				return fmt.Errorf("projection %q: read head: %w", r.p.Name, err)
			}
			head = max(head, next)
		}

		events := res.Events
		if r.segments > 1 {
			events = r.ownEvents(seg, events)
		}

		newCP := Checkpoint{
			Position:    next,
			Version:     r.p.Version,
			ReplayUntil: cp.ReplayUntil,
			Fingerprint: r.fingerprint,
		}
		phase := CatchingUp
		if res.AtEnd {
			phase = Live
		}
		status := Status{
			Phase:    phase,
			Position: next,
			Head:     head,
			Version:  r.p.Version,
			Owner:    r.p.Owner,
			Segment:  seg,
		}

		switch {
		case len(events) > 0:
			if err := r.applyBatch(ctx, seg, events, newCP, status); err != nil {
				return err
			}
			cp = newCP
		case next > cp.Position:
			// The read only scanned events this segment does not handle:
			// move the checkpoint past them, which also refreshes the claim.
			if err := r.commit(ctx, seg, newCP, status, func(context.Context) error { return nil }); err != nil {
				return err
			}
			cp = newCP
		default:
			// Nothing to persist; publish the phase so LoadStatuses sees it.
			// A lost claim drops the segment; any other failure only delays
			// what other instances see until the next commit.
			l := r.lease(seg)
			if err := r.store.SaveStatus(ctx, l, status); errors.Is(err, ErrClaimLost) {
				return err
			}
			r.setStatus(seg, l, status)
		}

		if res.AtEnd && r.segs[seg].rebuilding {
			r.segs[seg].rebuilding = false
			r.log(ctx, slog.LevelInfo, seg, next, "projection rebuild finished")
		}
		if res.AtEnd && !r.waitLive(ctx, seg) {
			return ctx.Err()
		}
	}
}

// reclaim refreshes the claim on seg after a reset and returns the
// segment's new checkpoint.
func (r *Runner) reclaim(ctx context.Context, seg int) (Checkpoint, error) {
	cp, ok, err := r.claim(ctx, seg)
	if err != nil {
		return Checkpoint{}, fmt.Errorf("projection %q: reclaim segment %d: %w", r.p.Name, seg, err)
	}
	if !ok {
		return Checkpoint{}, ErrClaimLost
	}
	return cp, nil
}

// rebuild resets the projection through [Store.Reset], every segment at
// once, and returns the checkpoint to continue seg from. The store releases
// the other segments, so this instance's own goroutines for them get
// ErrClaimLost on their next commit and claim them again. Before rebuild
// returns, every segment's in-memory status is back at position 0, so a
// [Runner.WaitUntil] after a rebuild waits for the replay rather than
// seeing the position reached before it.
func (r *Runner) rebuild(ctx context.Context, seg int) (Checkpoint, error) {
	r.log(ctx, slog.LevelInfo, seg, r.position(seg), "projection rebuild started")
	r.mu.Lock()
	before := make([]Lease, len(r.segs))
	for i, s := range r.segs {
		before[i] = s.lease
	}
	r.mu.Unlock()
	if err := r.store.Reset(ctx, r.lease(seg), r.p.Version, r.p.Reset); err != nil {
		return Checkpoint{}, fmt.Errorf("projection %q: rebuild for version %d: %w", r.p.Name, r.p.Version, err)
	}
	cp, err := r.reclaim(ctx, seg)
	if err != nil {
		return Checkpoint{}, err
	}
	r.mu.Lock()
	head := r.segs[seg].status.Head
	for i, s := range r.segs {
		if i == seg {
			continue
		}
		s.status.Position = 0
		s.status.Version = r.p.Version
		// The store released the segment. Retire the lease it was held
		// under, so a commit that reached the store before the reset
		// cannot write its wiped position back through setStatus. A
		// segment claimed again since keeps its new lease.
		if s.lease == before[i] {
			s.lease.Epoch = 0
		}
	}
	r.mu.Unlock()
	r.segs[seg].rebuilding = true
	// The replay runs every event through the handler again, so nothing
	// stays parked; Reset deleted the dead letters that parked it.
	r.segs[seg].parked = map[string]bool{}
	r.setStatus(seg, r.lease(seg), Status{
		Phase:    CatchingUp,
		Position: cp.Position,
		Head:     head,
		Version:  r.p.Version,
		Owner:    r.p.Owner,
		Segment:  seg,
	})
	r.notifyWatchers()
	return cp, nil
}

// ownEvents returns the events that belong to seg, reusing the backing
// array of events.
func (r *Runner) ownEvents(seg int, events []*eventsourcing.Envelope) []*eventsourcing.Envelope {
	own := events[:0]
	for _, env := range events {
		if SegmentOf(r.p.SegmentKey(env), r.segments) == seg {
			own = append(own, env)
		}
	}
	return own
}

// applyBatch runs the handler for events in one unit of work, committing
// the handler's writes and cp together. When the handler fails, it retries
// the events one at a time so the failing one can be stalled on or skipped
// on its own.
func (r *Runner) applyBatch(ctx context.Context, seg int, events []*eventsourcing.Envelope, cp Checkpoint, status Status) error {
	// A batch that touches a parked stream goes event by event, so the
	// parked stream's events can be skipped on their own.
	for _, env := range events {
		if r.segs[seg].parked[env.StreamID] {
			return r.applyOneByOne(ctx, seg, events, cp, status)
		}
	}
	err := r.commit(ctx, seg, cp, status, func(ctx context.Context) error {
		for _, env := range events {
			if err := r.handle(ctx, env, cp.ReplayUntil); err != nil {
				return err
			}
		}
		return nil
	})
	var he *handlerError
	if errors.As(err, &he) {
		return r.applyOneByOne(ctx, seg, events, cp, status)
	}
	if err != nil {
		return err
	}
	r.notifyWatchers()
	return nil
}

// applyOneByOne commits each event of a failed batch on its own, so the
// event that fails can be stalled on or skipped individually.
func (r *Runner) applyOneByOne(ctx context.Context, seg int, events []*eventsourcing.Envelope, batchCP Checkpoint, batchStatus Status) error {
	for _, env := range events {
		cp := batchCP
		cp.Position = env.GlobalPosition
		status := batchStatus
		status.Position = env.GlobalPosition

		if r.segs[seg].parked[env.StreamID] {
			if err := r.skipEvent(ctx, seg, env, ErrStreamParked, cp, status); err != nil {
				return err
			}
			continue
		}

		err := r.commit(ctx, seg, cp, status, func(ctx context.Context) error {
			return r.handle(ctx, env, cp.ReplayUntil)
		})
		var he *handlerError
		switch {
		case err == nil:
			// Applied on its own; a flaky handler may succeed on retry.
		case errors.As(err, &he) && r.shouldSkip(batchStatus.Phase, env.GlobalPosition <= cp.ReplayUntil):
			if err = r.skipEvent(ctx, seg, env, he.err, cp, status); err != nil {
				return err
			}
			r.segs[seg].parked[env.StreamID] = true
		default:
			return err
		}
	}
	r.notifyWatchers()
	return nil
}

// handle runs the projection's handler for env. A handler failure comes
// back as a *handlerError; a [eventsourcing.SkippedEventError] counts as
// success.
func (r *Runner) handle(ctx context.Context, env *eventsourcing.Envelope, replayUntil uint64) error {
	err := r.p.Handler.Handle(withEnvelope(ctx, env, replayUntil), env.Event)
	var skipped *eventsourcing.SkippedEventError
	if err == nil || errors.As(err, &skipped) {
		return nil
	}
	return &handlerError{env: env, err: err}
}

// skipEvent records a skipped event (if the store supports it) and moves
// the checkpoint past it.
func (r *Runner) skipEvent(ctx context.Context, seg int, env *eventsourcing.Envelope, cause error, cp Checkpoint, status Status) error {
	var eventType string
	if env.Event != nil {
		eventType = env.Event.EventType()
	}
	err := r.commit(ctx, seg, cp, status, func(ctx context.Context) error {
		dls, ok := r.store.(DeadLetterStore)
		if !ok {
			return nil
		}
		return dls.SaveDeadLetter(ctx, r.lease(seg), DeadLetter{
			Position:  env.GlobalPosition,
			StreamID:  env.StreamID,
			EventType: eventType,
			Error:     cause.Error(),
		})
	})
	if err != nil {
		return err
	}
	r.skipped.Add(1)
	r.log(ctx, slog.LevelWarn, seg, cp.Position, "projection event skipped",
		slog.String("stream", env.StreamID), slog.String("event_type", eventType), slog.String("error", cause.Error()))
	return nil
}

// loadParked reloads the streams parked in seg from the store's dead
// letters, so a parked stream stays parked across restarts and takeovers.
// Without a [DeadLetterStore] the runner keeps what it parked itself.
//
// A checkpoint at Position 0 has processed nothing since it was created or
// last reset, so no stream can be parked in it. Clearing the set there also
// unparks after a rebuild by another instance, which a store without dead
// letters cannot tell this one about.
func (r *Runner) loadParked(ctx context.Context, seg int, cp Checkpoint) error {
	if cp.Position == 0 {
		r.segs[seg].parked = map[string]bool{}
		return nil
	}
	dls, ok := r.store.(DeadLetterStore)
	if !ok {
		return nil
	}
	letters, err := dls.LoadDeadLetters(ctx, r.p.Name, seg)
	if err != nil {
		return fmt.Errorf("projection %q: load dead letters of segment %d: %w", r.p.Name, seg, err)
	}
	parked := make(map[string]bool, len(letters))
	for _, d := range letters {
		parked[d.StreamID] = true
	}
	r.segs[seg].parked = parked
	return nil
}

// replayStream applies the dead letters of streamID in seg again, each in
// a Commit that leaves the checkpoint at cp and removes the dead letter,
// and un-parks the stream once all of them apply.
func (r *Runner) replayStream(ctx context.Context, seg int, cp Checkpoint, streamID string) error {
	dls := r.store.(DeadLetterStore) // checked by ReplayStream
	letters, err := dls.LoadDeadLetters(ctx, r.p.Name, seg)
	if err != nil {
		return fmt.Errorf("projection %q: replay stream %q: load dead letters: %w", r.p.Name, streamID, err)
	}
	applied := make(map[uint64]bool)
	for _, d := range letters {
		// A position can carry more than one letter, as when a Commit that
		// recorded one failed after a store could not roll it back. The
		// first apply deletes them all; apply the event only once.
		if d.StreamID != streamID || applied[d.Position] {
			continue
		}
		applied[d.Position] = true
		res, err := r.reader.ReadAll(ctx, d.Position-1, eventsourcing.ReadOptions{Limit: 1})
		if err != nil {
			return fmt.Errorf("projection %q: replay stream %q: read event %d: %w", r.p.Name, streamID, d.Position, err)
		}
		if len(res.Events) != 1 || res.Events[0].GlobalPosition != d.Position {
			return fmt.Errorf("projection %q: replay stream %q: event %d is not in the log", r.p.Name, streamID, d.Position)
		}
		env := res.Events[0]

		r.mu.Lock()
		status := r.segs[seg].status
		r.mu.Unlock()
		err = r.commit(ctx, seg, cp, status, func(ctx context.Context) error {
			if err := r.handle(ctx, env, cp.ReplayUntil); err != nil {
				return err
			}
			return dls.DeleteDeadLetter(ctx, r.lease(seg), d.Position)
		})
		if err != nil {
			return fmt.Errorf("projection %q: replay stream %q at %d: %w", r.p.Name, streamID, d.Position, err)
		}
	}
	delete(r.segs[seg].parked, streamID)
	r.notifyWatchers()
	return nil
}

// shouldSkip reports whether a failing event is skipped under
// [Definition.OnError]. phase is the phase of the read that returned the
// event, not the one last published: after a restart or a stall, a read
// that reaches the head of the log is Live even though the segment has not
// reported Live since.
func (r *Runner) shouldSkip(phase Phase, replaying bool) bool {
	switch r.p.OnError {
	case SkipAlways:
		return true
	case SkipWhenLive:
		return phase == Live && !replaying
	default:
		return false
	}
}

// handlerError marks an error that came from the projection's handler
// rather than from the store, so applyBatch can tell them apart.
type handlerError struct {
	env *eventsourcing.Envelope
	err error
}

func (e *handlerError) Error() string {
	return fmt.Sprintf("projection: handler failed on event %d: %v", e.env.GlobalPosition, e.err)
}

func (e *handlerError) Unwrap() error { return e.err }

// commit wraps Store.Commit and updates the in-memory status on success.
func (r *Runner) commit(ctx context.Context, seg int, cp Checkpoint, status Status, fn func(ctx context.Context) error) error {
	l := r.lease(seg)
	if err := r.store.Commit(ctx, l, cp, status, fn); err != nil {
		return err
	}
	r.setStatus(seg, l, status)
	return nil
}

// waitLive blocks seg at the head of the log until a nudge or the poll
// interval elapses. It reports false if ctx is done.
func (r *Runner) waitLive(ctx context.Context, seg int) bool {
	t := time.NewTimer(r.p.PollInterval)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-r.segs[seg].nudge:
	case <-t.C:
	}
	return true
}

// publishStatus saves seg's in-memory status to the store, so other
// instances see a stall instead of the phase of the last commit. The
// claim is still held after a failed batch, as only a lost claim ends it.
// Best effort: the store may be what failed, and ErrClaimLost means
// another instance owns the segment now, which the next Claim finds out.
func (r *Runner) publishStatus(ctx context.Context, seg int) {
	r.mu.Lock()
	st := r.segs[seg].status
	r.mu.Unlock()
	_ = r.store.SaveStatus(ctx, r.lease(seg), st)
}

// sleepCtx waits for d or until ctx is done, whichever comes first.
func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// newRetryBackOff returns the capped exponential delay used while a segment
// is stalled on repeated errors. It never gives up: a stalled segment keeps
// retrying until ctx is done. Correctness never depends on its values:
// correctness comes from Commit's unit-of-work atomicity.
func newRetryBackOff() *backoff.ExponentialBackOff {
	return backoff.NewExponentialBackOff(
		backoff.WithInitialInterval(500*time.Millisecond),
		backoff.WithMultiplier(2),
		backoff.WithMaxInterval(30*time.Second),
		backoff.WithMaxElapsedTime(0),
	)
}
