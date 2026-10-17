package projection

import (
	"context"
	"time"
)

// Status returns the projection's status as seen by this instance.
//
// For a segmented projection the returned status combines its segments:
// Phase is the furthest-behind phase of any segment (Stalled, then
// Starting/Standby, CatchingUp, Live); Position is the lowest; Head the
// highest; LastError and Owner those of the lowest-numbered segment that
// has one.
func (r *Runner) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := Status{
		Phase:    Live,
		Position: r.segs[0].status.Position,
		Version:  r.p.Version,
	}
	for _, s := range r.segs {
		out.Position = min(out.Position, s.status.Position)
		out.Head = max(out.Head, s.status.Head)
		if phaseRank(s.status.Phase) < phaseRank(out.Phase) {
			out.Phase = s.status.Phase
		}
		if out.LastError == "" {
			out.LastError = s.status.LastError
		}
		if out.Owner == "" {
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
	case CatchingUp:
		return 2
	case Live:
		return 3
	default: // Starting, Standby
		return 1
	}
}

// IsLive reports whether s, as returned by [Store.LoadStatuses], shows a
// live segment: caught up with the head of the log, held by an owner, and
// published no longer than claimTTL ago. claimTTL is the
// [Projection.ClaimTTL] the projection runs with; a live owner publishes
// its status at least every [Projection.PollInterval], so a status older
// than its claim is one whose owner stopped without releasing it.
func (s Status) IsLive(claimTTL time.Duration) bool {
	return s.Phase == Live && s.Owner != "" && s.Age <= claimTTL
}

// Live reports whether the projection is live: caught up with the head of
// the log on every segment, on whichever instance owns each segment.
//
// On segments this instance owns, Live answers from memory. On segments
// another instance owns, it reads [Store.LoadStatuses] and judges each with
// [Status.IsLive], so a segment whose owner stopped or crashed is not live.
func (r *Runner) Live(ctx context.Context) (bool, error) {
	if r.segments == 1 {
		r.mu.Lock()
		st := r.segs[0].status
		r.mu.Unlock()
		if st.Owner == r.p.Owner {
			return st.Phase == Live, nil
		}
	}
	statuses, err := r.store.LoadStatuses(ctx, r.p.Name)
	if err != nil {
		return false, err
	}
	if len(statuses) < r.segments {
		return false, nil
	}
	for _, s := range statuses {
		if !s.IsLive(r.p.ClaimTTL) {
			return false, nil
		}
	}
	return true, nil
}

// WaitUntil blocks until the projection has processed the log up to and
// including position on every segment, or until ctx is done.
//
// position is an [eventsourcing.Envelope.GlobalPosition], typically from the
// [eventsourcing.AppendResult] of the command whose effects the caller
// wants to see.
//
// Events the projection does not handle count as processed. On segments
// this instance owns, WaitUntil watches memory; on segments another
// instance owns, it polls [Store.LoadStatuses] at [Projection.PollInterval].
func (r *Runner) WaitUntil(ctx context.Context, position uint64) error {
	for {
		// Take the change signal before checking, so a change that lands
		// between the check and the wait still wakes us.
		r.mu.Lock()
		changed := r.changed
		r.mu.Unlock()

		ok, allLocal, err := r.reached(ctx, position)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}

		// Segments owned elsewhere only change in the store; poll for them.
		var (
			poll  *time.Timer
			pollC <-chan time.Time
		)
		if !allLocal {
			poll = time.NewTimer(r.p.PollInterval)
			pollC = poll.C
		}
		select {
		case <-ctx.Done():
		case <-changed:
		case <-pollC:
		}
		if poll != nil {
			poll.Stop()
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

// reached reports whether every segment has processed the log up to
// position, and whether this instance owns every segment.
func (r *Runner) reached(ctx context.Context, position uint64) (ok, allLocal bool, err error) {
	r.mu.Lock()
	allLocal = true
	for _, s := range r.segs {
		if s.status.Owner != r.p.Owner {
			allLocal = false
			continue
		}
		if s.status.Position < position {
			r.mu.Unlock()
			return false, allLocal, nil
		}
	}
	r.mu.Unlock()
	if allLocal {
		return true, true, nil
	}

	statuses, err := r.store.LoadStatuses(ctx, r.p.Name)
	if err != nil {
		return false, false, err
	}
	if len(statuses) < r.segments {
		return false, false, nil
	}
	for _, s := range statuses {
		if s.Position < position {
			return false, false, nil
		}
	}
	return true, false, nil
}

// setStatus replaces seg's status after a commit or a status publish.
func (r *Runner) setStatus(seg int, s Status) {
	r.mu.Lock()
	r.segs[seg].status = s
	r.signalChange()
	r.mu.Unlock()
}

// setPhase moves seg to phase p, recording lastErr for [Stalled], and
// notifies watchers.
func (r *Runner) setPhase(seg int, p Phase, lastErr string) {
	r.mu.Lock()
	st := &r.segs[seg].status
	st.Phase = p
	st.LastError = lastErr
	if p == Standby {
		st.Owner = ""
	} else {
		st.Owner = r.p.Owner
	}
	r.signalChange()
	r.mu.Unlock()
	r.notifyWatchers()
}

// signalChange wakes every WaitUntil. r.mu must be held.
func (r *Runner) signalChange() {
	close(r.changed)
	r.changed = make(chan struct{})
}
