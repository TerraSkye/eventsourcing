package projection

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"
)

// ReplayStream applies the skipped events of streamID again, in order, and
// un-parks the stream once they all apply. Call it after fixing whatever
// made the handler fail. Each event is applied and its dead letter removed
// in one Commit; if the handler fails again, the stream stays parked, with
// the events not yet applied still recorded.
//
// Like [Runner.Rebuild], it records a request in the store with
// [DeadLetterStore.RequestReplay], for every segment that holds dead
// letters of the stream. Whichever instance holds each of those segments
// serves it between two batches: at once when that is this instance,
// otherwise within its [Definition.PollInterval]. So it works from any
// instance, whichever segments it holds, and the request survives restarts
// and claims moving between instances. ReplayStream then waits until every
// one of those segments has tried it, and returns the errors they failed
// with, joined. An error from a segment on this instance comes back as it
// was, for [errors.Is] and [errors.As]; one from a segment on another
// instance only as its text. A stream with nothing to replay returns nil
// at once.
//
// ctx only bounds how long ReplayStream waits: the request is kept in the
// store and still served when ctx ends first. While no instance runs the
// projection, ReplayStream waits until ctx ends. It returns
// [ErrNoDeadLetterStore] when the Store keeps no dead letters, and an error
// wrapping [ErrUnknownProjection] before any runner declared the
// projection.
func (r *Runner) ReplayStream(ctx context.Context, streamID string) error {
	dls, ok := r.store.(DeadLetterStore)
	if !ok {
		return ErrNoDeadLetterStore
	}
	r.mu.Lock()
	r.replayWaiters++
	r.mu.Unlock()
	var n uint64
	defer func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.replayErrs, n)
		if r.replayWaiters--; r.replayWaiters == 0 {
			clear(r.replayErrs)
		}
	}()
	n, err := dls.RequestReplay(ctx, r.p.Name, streamID)
	if err != nil {
		return fmt.Errorf("projection %q: request replay of stream %q: %w", r.p.Name, streamID, err)
	}
	select {
	case r.checkReplays <- struct{}{}:
	default: // a check is already pending
	}

	tick := time.NewTicker(r.p.PollInterval)
	defer tick.Stop()
	for {
		r.mu.Lock()
		replayed := r.replayed
		r.mu.Unlock()
		pending, msgs, err := dls.ReplayResult(ctx, r.p.Name, n)
		if err != nil {
			return fmt.Errorf("projection %q: replay of stream %q: read result: %w", r.p.Name, streamID, err)
		}
		if pending == 0 {
			return r.replayError(n, msgs)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-replayed: // a segment here finished a replay
		case <-tick.C: // one on another instance only shows in the store
		}
	}
}

// replayError joins the errors msgs that the segments failed request n
// with. Each one a segment on this instance failed with is returned as it
// was; the others are rebuilt from their text.
func (r *Runner) replayError(n uint64, msgs []string) error {
	r.mu.Lock()
	local := slices.Clone(r.replayErrs[n])
	r.mu.Unlock()
	errs := make([]error, len(msgs))
	for i, msg := range msgs {
		j := slices.IndexFunc(local, func(err error) bool { return err.Error() == msg })
		if j < 0 {
			errs[i] = errors.New(msg)
			continue
		}
		errs[i] = local[j]
		local = slices.Delete(local, j, j+1)
	}
	return errors.Join(errs...)
}

// runReplays queues the replay requests pending in the store for the
// segments this instance holds, until ctx is done. One read covers every
// segment of the projection. It reads at once after a ReplayStream on this
// instance, and otherwise once per PollInterval while [Runner.replaysWanted]
// says a request may be pending here; a projection without dead letters
// costs no reads. A failed read is logged once, until a read succeeds, and
// tried again at the next poll.
func (r *Runner) runReplays(ctx context.Context) {
	dls, ok := r.store.(DeadLetterStore)
	if !ok {
		return
	}
	t := time.NewTicker(r.p.PollInterval)
	defer t.Stop()
	failing := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !r.replaysWanted() {
				continue
			}
		case <-r.checkReplays:
		}
		err := r.queueReplays(ctx, dls)
		switch {
		case err != nil && ctx.Err() == nil && !failing:
			r.p.Logger.WarnContext(ctx, "projection replay poll failed; requested replays wait",
				slog.String("projection", r.p.Name), slog.String("owner", r.p.Owner), slog.String("error", err.Error()))
			failing = true
		case err == nil && failing:
			r.p.Logger.InfoContext(ctx, "projection replay poll recovered",
				slog.String("projection", r.p.Name), slog.String("owner", r.p.Owner))
			failing = false
		}
	}
}

// replaysWanted reports whether the regular poll should read the store:
// while a segment here holds dead letters, as only those get requests, and
// once after a segment here was claimed or served a replay, for a request
// whose dead letters an instance before it already applied, or one made
// while it served another.
func (r *Runner) replaysWanted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pollReplays || slices.ContainsFunc(r.segs, func(s *segmentState) bool { return s.active && s.hasParked })
}

// noteParked records whether seg has parked streams, for
// [Runner.replaysWanted]. The segment's goroutine calls it after each
// change to parked.
func (r *Runner) noteParked(seg int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.segs[seg].hasParked = len(r.segs[seg].parked) > 0
}

// queueReplays reads the pending replay requests and queues each one for
// its segment when this instance processes that segment and has not queued
// it yet.
func (r *Runner) queueReplays(ctx context.Context, dls DeadLetterStore) error {
	// Cleared before the read, so a segment claimed meanwhile asks again.
	r.mu.Lock()
	r.pollReplays = false
	r.mu.Unlock()
	pending, err := dls.PendingReplays(ctx, r.p.Name)
	if err != nil {
		r.mu.Lock()
		r.pollReplays = true
		r.mu.Unlock()
		return fmt.Errorf("projection %q: load replay requests: %w", r.p.Name, err)
	}
	r.mu.Lock()
	var wake []int
	for _, rp := range pending {
		if rp.Segment < 0 || rp.Segment >= len(r.segs) || !r.segs[rp.Segment].active || r.queuedReplays[rp] {
			continue
		}
		r.queuedReplays[rp] = true
		r.pendingReplays[rp.Segment] = append(r.pendingReplays[rp.Segment], rp)
		wake = append(wake, rp.Segment)
	}
	r.mu.Unlock()
	for _, seg := range wake {
		select {
		case r.segs[seg].nudge <- struct{}{}:
		default: // a nudge is already pending
		}
	}
	return nil
}

// serveReplays replays the requests queued for seg, between two batches,
// and records each outcome with [DeadLetterStore.FinishReplay]. A request
// cut short because the claim was lost or ctx ended is left pending in the
// store, for whichever instance holds seg next; the dead letters it already
// applied are gone, so that one does not apply them twice.
func (r *Runner) serveReplays(ctx context.Context, seg int, cp Checkpoint) error {
	reqs := r.takeReplays(seg)
	if len(reqs) == 0 {
		return nil
	}
	dls := r.store.(DeadLetterStore) // checked by runReplays
	defer r.dropReplays(reqs)
	defer func() {
		r.mu.Lock()
		r.pollReplays = true
		r.mu.Unlock()
	}()
	for _, rp := range reqs {
		err := r.replayStream(ctx, seg, cp, rp.StreamID)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, ErrClaimLost) {
			return err
		}
		var msg string
		if err != nil {
			msg = err.Error()
			r.mu.Lock()
			if r.replayWaiters > 0 {
				r.replayErrs[rp.Number] = append(r.replayErrs[rp.Number], err)
			}
			r.mu.Unlock()
		}
		// A failed FinishReplay other than a lost claim leaves the request
		// pending: the next poll queues it again, and with its dead letters
		// applied the next try only records the outcome.
		if err := dls.FinishReplay(ctx, r.lease(seg), rp.Number, msg); errors.Is(err, ErrClaimLost) {
			return err
		}
		r.mu.Lock()
		close(r.replayed)
		r.replayed = make(chan struct{})
		r.mu.Unlock()
	}
	return nil
}

// takeReplays returns the replay requests queued for seg and clears them.
// They stay in queuedReplays until [Runner.dropReplays].
func (r *Runner) takeReplays(seg int) []Replay {
	r.mu.Lock()
	defer r.mu.Unlock()
	reqs := r.pendingReplays[seg]
	delete(r.pendingReplays, seg)
	return reqs
}

// dropReplays forgets that reqs were queued, so a later poll queues any of
// them still pending in the store again.
func (r *Runner) dropReplays(reqs []Replay) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rp := range reqs {
		delete(r.queuedReplays, rp)
	}
}

// setActive records whether this instance is processing seg. When seg stops
// being active, the replay requests queued for it are dropped: they stay
// pending in the store for whichever instance holds seg next.
func (r *Runner) setActive(seg int, active bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.segs[seg].active = active
	if active {
		r.pollReplays = true
		return
	}
	for _, rp := range r.pendingReplays[seg] {
		delete(r.queuedReplays, rp)
	}
	delete(r.pendingReplays, seg)
}
