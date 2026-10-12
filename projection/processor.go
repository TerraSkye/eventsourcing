package projection

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/terraskye/eventsourcing"
)

// runSegment is the loop for one segment of the projection. It is the only
// loop in this package: a single-segment projection runs it once, a
// segmented projection runs it once per segment.
//
// The loop:
//
//  1. tries to claim the segment from the store;
//  2. if claimed, loads the checkpoint and processes events in batches,
//     each batch committed through Store.Commit so read-model writes and
//     checkpoint moves land together;
//  3. if not claimed, reports Standby and polls until the claim becomes
//     available (another instance owned it and its claim expired, or we
//     caught the next retry window);
//  4. on commit, refreshes the claim as a side effect; on ClaimLost the
//     segment is dropped and the loop goes back to step 1;
//  5. on ctx.Done, releases the claim so another instance can take over
//     immediately rather than waiting the claim TTL out.
func (r *Runner) runSegment(ctx context.Context, seg int) {
	// Release on exit, regardless of why.
	defer func() {
		relCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = r.store.Release(relCtx, r.p.Name, seg, r.p.Owner)
	}()

	backoff := newBackoff()

	for {
		if ctx.Err() != nil {
			return
		}
		cp, ok, err := r.store.Claim(ctx, r.p.Name, seg, r.p.Owner, r.p.ClaimTTL)
		if err != nil {
			r.setPhase(seg, Stalled, err.Error())
			if !sleepCtx(ctx, backoff.next()) {
				return
			}
			continue
		}
		if !ok {
			r.setStandby(seg)
			if !sleepCtx(ctx, r.p.PollInterval) {
				return
			}
			continue
		}
		backoff.reset()

		// Held the claim. Run until it is lost (reclaim, ctx.Done, or a
		// non-recoverable error).
		if err := r.work(ctx, seg, cp); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			if errors.Is(err, ErrClaimLost) {
				// Someone else took the segment; go back to Standby.
				continue
			}
			// Other errors: record and back off, keep the claim if we can.
			r.setPhase(seg, Stalled, err.Error())
			if !sleepCtx(ctx, backoff.next()) {
				return
			}
			continue
		}
	}
}

// work holds a claim and processes events on it. It returns when the claim
// is lost, ctx is done, or a non-recoverable error happens.
func (r *Runner) work(ctx context.Context, seg int, cp Checkpoint) error {
	// Rebuild if the stored version differs from the configured one AND
	// there is already something to wipe. A fresh segment (nothing
	// processed yet) just picks up the configured version on its first
	// commit.
	if r.p.Version != 0 && cp.Version != r.p.Version && cp.Position > 0 {
		if r.p.Reset == nil {
			return fmt.Errorf("projection %s: stored version %d differs from configured %d but no Reset is set", r.p.Name, cp.Version, r.p.Version)
		}
		if err := r.doReset(ctx, seg); err != nil {
			return err
		}
		// Reload after reset.
		var ok bool
		var err error
		cp, ok, err = r.store.Claim(ctx, r.p.Name, seg, r.p.Owner, r.p.ClaimTTL)
		if err != nil {
			return err
		}
		if !ok {
			return ErrClaimLost
		}
	}

	// Capture the head at claim time, so events already in the log count
	// as replayed (IsReplaying) and new ones do not. This only matters the
	// first time; afterwards cp.ReplayUntil is persisted.
	if cp.ReplayUntil == 0 && cp.Position == 0 {
		head, err := r.reader.Head(ctx)
		if err != nil {
			return err
		}
		cp.ReplayUntil = head
	}

	notify := r.notifyChannel(ctx)

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Serve a pending rebuild on segment 0 of an unsegmented
		// projection, between batches.
		if r.segments == 1 && seg == 0 {
			select {
			case req := <-r.rebuilds:
				err := r.handleRebuild(req.ctx, seg)
				req.done <- err
				if err != nil {
					return err
				}
				// Reload after rebuild.
				var ok bool
				var e error
				cp, ok, e = r.store.Claim(ctx, r.p.Name, seg, r.p.Owner, r.p.ClaimTTL)
				if e != nil {
					return e
				}
				if !ok {
					return ErrClaimLost
				}
				head, e := r.reader.Head(ctx)
				if e != nil {
					return e
				}
				cp.ReplayUntil = head
			default:
			}
		}

		res, err := r.reader.ReadAll(ctx, cp.Position, r.readOptions())
		if err != nil {
			return err
		}

		// Filter events to this segment.
		events := res.Events
		if r.segments > 1 {
			filtered := events[:0]
			for _, env := range events {
				if SegmentOf(r.p.SegmentKey(env), r.segments) == seg {
					filtered = append(filtered, env)
				}
			}
			events = filtered
		}

		next := res.Next
		if next <= cp.Position {
			next = cp.Position
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
			Position: newCP.Position,
			Head:     next,
			Version:  r.p.Version,
			Owner:    r.p.Owner,
			Segment:  seg,
		}

		if len(events) == 0 {
			// Still refresh the claim and move the checkpoint if the read
			// scanned past some events (filter-only pages).
			if next > cp.Position {
				if err := r.commit(ctx, seg, newCP, status, func(ctx context.Context) error { return nil }); err != nil {
					return err
				}
				cp = newCP
			} else {
				// Nothing to persist; just refresh status via SaveStatus so
				// LoadStatuses sees up-to-date Phase.
				_ = r.store.SaveStatus(ctx, r.p.Name, seg, r.p.Owner, status)
				r.setStatus(seg, status)
			}
		} else {
			if err := r.applyBatch(ctx, seg, events, newCP, status, cp.ReplayUntil); err != nil {
				return err
			}
			cp = newCP
		}

		if res.AtEnd {
			// At the head — wait for a nudge, a notification, or a poll.
			if !r.waitLive(ctx, notify) {
				return ctx.Err()
			}
		}
	}
}

// applyBatch runs the handlers for events in one unit of work, committing
// the handlers' writes and the new checkpoint together.
func (r *Runner) applyBatch(ctx context.Context, seg int, events []*eventsourcing.Envelope, cp Checkpoint, status Status, replayUntil uint64) error {
	err := r.commit(ctx, seg, cp, status, func(ctx context.Context) error {
		for _, env := range events {
			ctx := withEnvelope(ctx, env, replayUntil)
			if err := r.p.Handlers.Handle(ctx, env.Event); err != nil {
				if isSkipped(err) {
					continue
				}
				return &handlerError{env: env, err: err}
			}
		}
		return nil
	})
	if err == nil {
		r.notifyWatchers()
		return nil
	}

	// If the batch failed because of a handler, decide what to do about
	// that one event based on OnError.
	var he *handlerError
	if errors.As(err, &he) {
		return r.handleFailedBatch(ctx, seg, events, cp, status, replayUntil, he)
	}
	// Store error (ClaimLost etc.) — pass through.
	return err
}

// handleFailedBatch retries the batch one event at a time, so a failing
// event can be stalled on or skipped individually.
func (r *Runner) handleFailedBatch(ctx context.Context, seg int, events []*eventsourcing.Envelope, batchCP Checkpoint, batchStatus Status, replayUntil uint64, firstFailure *handlerError) error {
	for _, env := range events {
		// Try this event on its own.
		singleCP := Checkpoint{
			Position:    env.GlobalVersion,
			Version:     batchCP.Version,
			ReplayUntil: batchCP.ReplayUntil,
		}
		singleStatus := batchStatus
		singleStatus.Position = env.GlobalVersion
		singleStatus.Head = env.GlobalVersion

		var handlerErr *handlerError
		err := r.commit(ctx, seg, singleCP, singleStatus, func(ctx context.Context) error {
			ctx = withEnvelope(ctx, env, replayUntil)
			if err := r.p.Handlers.Handle(ctx, env.Event); err != nil {
				if isSkipped(err) {
					return nil
				}
				return &handlerError{env: env, err: err}
			}
			return nil
		})
		if err == nil {
			// Event applied cleanly after all (e.g. a flaky handler succeeded on retry).
			continue
		}
		_ = handlerErr
		if errors.As(err, &handlerErr) {
			if r.shouldSkip(seg, env.GlobalVersion <= replayUntil) {
				if err := r.skipEvent(ctx, seg, env, handlerErr.err, singleCP, singleStatus); err != nil {
					return err
				}
				continue
			}
			return handlerErr
		}
		// Store error — surface.
		return err
	}
	_ = firstFailure // kept for symmetry; the per-event retry re-discovers the failure.
	r.notifyWatchers()
	return nil
}

// skipEvent records a skipped event (if the store supports it) and moves
// the checkpoint past it.
func (r *Runner) skipEvent(ctx context.Context, seg int, env *eventsourcing.Envelope, cause error, cp Checkpoint, status Status) error {
	eventType := ""
	if env.Event != nil {
		eventType = env.Event.EventType()
	}
	return r.commit(ctx, seg, cp, status, func(ctx context.Context) error {
		if dls, ok := r.store.(DeadLetterStore); ok {
			if err := dls.SaveDeadLetter(ctx, r.p.Name, seg, env.GlobalVersion, eventType, cause); err != nil {
				return err
			}
		} else {
			r.p.Logger.Error("projection: skipping failing event",
				"projection", r.p.Name, "segment", seg,
				"position", env.GlobalVersion, "event_type", eventType, "err", cause)
		}
		atomic.AddInt64(&r.skippedCount, 1)
		return nil
	})
}

func (r *Runner) shouldSkip(seg int, replaying bool) bool {
	switch r.p.OnError {
	case SkipAlways:
		return true
	case SkipWhenLive:
		r.mu.Lock()
		live := r.segs[seg].status.Phase == Live
		r.mu.Unlock()
		return live && !replaying
	}
	return false
}

// handlerError marks an error that came from a projection handler rather
// than from the store, so handleFailedBatch can tell them apart.
type handlerError struct {
	env *eventsourcing.Envelope
	err error
}

func (e *handlerError) Error() string {
	return fmt.Sprintf("projection: handler failed on event %d: %v", e.env.GlobalVersion, e.err)
}

func (e *handlerError) Unwrap() error { return e.err }

func isSkipped(err error) bool {
	var skipped *eventsourcing.SkippedEventError
	return errors.As(err, &skipped)
}

// commit wraps Store.Commit and updates in-memory status on success.
func (r *Runner) commit(ctx context.Context, seg int, cp Checkpoint, status Status, fn func(ctx context.Context) error) error {
	err := r.store.Commit(ctx, r.p.Name, seg, r.p.Owner, cp, status, fn)
	if err != nil {
		return err
	}
	r.setStatus(seg, status)
	return nil
}

// waitLive blocks at the head of the log until a nudge, a store
// notification, or the poll interval elapses. Returns false if ctx was
// cancelled.
func (r *Runner) waitLive(ctx context.Context, notify <-chan struct{}) bool {
	// A segmented reader waits on the same signals as an unsegmented one;
	// a nudge to any segment is a nudge to the shared Runner.
	timer := time.NewTimer(r.p.PollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-r.nudge:
	case <-timer.C:
	case <-notify:
	}
	return true
}

// notifyChannel returns a channel that receives whenever the reader signals
// new appends. If the reader doesn't implement Notifier, the channel never
// receives.
func (r *Runner) notifyChannel(ctx context.Context) <-chan struct{} {
	if n, ok := r.reader.(eventsourcing.Notifier); ok {
		ch, err := n.Notify(ctx)
		if err == nil {
			return ch
		}
	}
	// A nil channel never selects; a closed channel always does. We want
	// never — a blocked select arm.
	return nil
}

func (r *Runner) setStatus(seg int, s Status) {
	r.mu.Lock()
	st := r.segs[seg]
	st.status = s
	close(st.changed)
	st.changed = make(chan struct{})
	r.mu.Unlock()
}

func (r *Runner) setPhase(seg int, p Phase, lastErr string) {
	r.mu.Lock()
	st := r.segs[seg]
	st.status.Phase = p
	st.status.LastError = lastErr
	if p == Standby {
		st.status.Owner = ""
	} else {
		st.status.Owner = r.p.Owner
	}
	close(st.changed)
	st.changed = make(chan struct{})
	r.mu.Unlock()
	r.notifyWatchers()
}

func (r *Runner) setStandby(seg int) {
	r.setPhase(seg, Standby, "")
}

// doReset handles the Version-change rebuild case (automatic rebuild on
// start). Rebuild via Runner.Rebuild goes through handleRebuild.
func (r *Runner) doReset(ctx context.Context, seg int) error {
	err := r.store.Reset(ctx, r.p.Name, seg, r.p.Owner, r.p.Version, r.p.Reset)
	if err != nil {
		return err
	}
	r.notifyWatchers()
	return nil
}

// handleRebuild performs a user-requested rebuild on an unsegmented
// projection.
func (r *Runner) handleRebuild(ctx context.Context, seg int) error {
	if r.p.Reset == nil {
		return ErrNoReset
	}
	return r.store.Reset(ctx, r.p.Name, seg, r.p.Owner, r.p.Version, r.p.Reset)
}

// Skipped returns the number of events skipped under a non-stalling
// OnError since Run started.
func (r *Runner) Skipped() int64 { return atomic.LoadInt64(&r.skippedCount) }

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// backoff implements a simple capped exponential delay used while a segment
// is stalled on repeated errors. Correctness never depends on its values:
// correctness comes from Commit's unit-of-work atomicity.
type backoff struct {
	cur time.Duration
}

func newBackoff() *backoff { return &backoff{} }

func (b *backoff) next() time.Duration {
	if b.cur == 0 {
		b.cur = 500 * time.Millisecond
	} else {
		b.cur *= 2
		if b.cur > 30*time.Second {
			b.cur = 30 * time.Second
		}
	}
	return b.cur
}

func (b *backoff) reset() { b.cur = 0 }
