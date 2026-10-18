package projection

import (
	"context"
	"errors"
	"fmt"
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
	defer func() {
		// ctx is done by now; release under a context that is not.
		relCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
		defer cancel()
		// Best effort: if Release fails, the claim expires after ClaimTTL
		// and another instance takes over then.
		_ = r.store.Release(relCtx, r.lease(seg))
	}()

	retry := newRetryBackOff()
	for ctx.Err() == nil {
		cp, ok, err := r.claim(ctx, seg)
		if err != nil {
			r.setPhase(seg, Stalled, err.Error())
			sleepCtx(ctx, retry.NextBackOff())
			continue
		}
		if !ok {
			r.setPhase(seg, Standby, "")
			sleepCtx(ctx, r.p.PollInterval)
			continue
		}
		retry.Reset()

		r.setActive(seg, true)
		err = r.work(ctx, seg, cp)
		r.setActive(seg, false)
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, ErrClaimLost):
			// Another instance took the segment; compete for it again.
		default:
			r.setPhase(seg, Stalled, err.Error())
			sleepCtx(ctx, retry.NextBackOff())
		}
	}
}

// work holds the claim on seg and processes events from cp. It returns
// when the claim is lost, ctx is done, or a batch fails; it never returns
// nil.
func (r *Runner) work(ctx context.Context, seg int, cp Checkpoint) error {
	// Rebuild if the stored version differs from the configured one and
	// there is already something to wipe. A fresh segment (nothing
	// processed yet) just picks up the configured version on its first
	// commit.
	if r.p.Version != 0 && cp.Version != r.p.Version && cp.Position > 0 {
		var err error
		if cp, err = r.rebuild(ctx, seg); err != nil {
			return err
		}
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		if req := r.takeRebuild(seg); req != nil {
			var err error
			cp, err = r.rebuild(ctx, seg)
			req.finish(err)
			if err != nil {
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
			if err := r.store.SaveStatus(ctx, r.lease(seg), status); errors.Is(err, ErrClaimLost) {
				return err
			}
			r.setStatus(seg, status)
		}

		if res.AtEnd && !r.waitLive(ctx) {
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

// rebuild resets seg through [Store.Reset] and returns the checkpoint to
// continue from. Before it returns, the in-memory status is back at
// position 0, so a [Runner.WaitUntil] after a rebuild waits for the replay
// rather than seeing the position reached before it.
func (r *Runner) rebuild(ctx context.Context, seg int) (Checkpoint, error) {
	if err := r.store.Reset(ctx, r.lease(seg), r.p.Version, r.p.Reset); err != nil {
		return Checkpoint{}, fmt.Errorf("projection %q: rebuild for version %d: %w", r.p.Name, r.p.Version, err)
	}
	cp, err := r.reclaim(ctx, seg)
	if err != nil {
		return Checkpoint{}, err
	}
	r.mu.Lock()
	head := r.segs[seg].status.Head
	r.mu.Unlock()
	r.setStatus(seg, Status{
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
		cp.Position = env.GlobalVersion
		status := batchStatus
		status.Position = env.GlobalVersion

		err := r.commit(ctx, seg, cp, status, func(ctx context.Context) error {
			return r.handle(ctx, env, cp.ReplayUntil)
		})
		var he *handlerError
		switch {
		case err == nil:
			// Applied on its own; a flaky handler may succeed on retry.
		case errors.As(err, &he) && r.shouldSkip(batchStatus.Phase, env.GlobalVersion <= cp.ReplayUntil):
			if err = r.skipEvent(ctx, seg, env, he.err, cp, status); err != nil {
				return err
			}
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
		return dls.SaveDeadLetter(ctx, r.lease(seg), env.GlobalVersion, eventType, cause)
	})
	if err != nil {
		return err
	}
	r.skipped.Add(1)
	return nil
}

// shouldSkip reports whether a failing event is skipped under
// [Projection.OnError]. phase is the phase of the read that returned the
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
	return fmt.Sprintf("projection: handler failed on event %d: %v", e.env.GlobalVersion, e.err)
}

func (e *handlerError) Unwrap() error { return e.err }

// commit wraps Store.Commit and updates the in-memory status on success.
func (r *Runner) commit(ctx context.Context, seg int, cp Checkpoint, status Status, fn func(ctx context.Context) error) error {
	if err := r.store.Commit(ctx, r.lease(seg), cp, status, fn); err != nil {
		return err
	}
	r.setStatus(seg, status)
	return nil
}

// waitLive blocks at the head of the log until a nudge or the poll
// interval elapses. It reports false if ctx is done.
func (r *Runner) waitLive(ctx context.Context) bool {
	t := time.NewTimer(r.p.PollInterval)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-r.nudge:
	case <-t.C:
	}
	return true
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
