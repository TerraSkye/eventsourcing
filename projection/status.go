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
// [Definition.ClaimTTL] the projection runs with; a live owner publishes
// its status at least every [Definition.PollInterval], so a status older
// than its claim is one whose owner stopped without releasing it.
func (s Status) IsLive(claimTTL time.Duration) bool {
	return s.Phase == Live && s.Owner != "" && s.Age <= claimTTL
}

// IsLive reports whether the projection is live: caught up with the head of
// the log on every segment, on whichever instance owns each segment.
//
// On segments this instance owns, IsLive answers from memory. On segments
// another instance owns, it reads [Store.LoadStatuses] and judges each with
// [Status.IsLive], so a segment whose owner stopped or crashed is not live.
func (r *Runner) IsLive(ctx context.Context) (bool, error) {
	if r.segments == 1 {
		r.mu.Lock()
		st := r.segs[0].status
		r.mu.Unlock()
		if st.Owner == r.p.Owner {
			return st.Phase == Live, nil
		}
	}
	return r.probe.IsLive(ctx)
}

// WaitUntil blocks until the projection has processed the log up to and
// including position on every segment, or until ctx is done.
//
// position is an [eventsourcing.Envelope.GlobalPosition], typically from the
// [eventsourcing.AppendResult] of the command whose effects the caller
// wants to see.
//
// Events the projection does not handle count as processed. While this
// instance owns every segment, WaitUntil watches memory. Otherwise it waits
// on the store like a [Probe], at [Definition.PollInterval], sharing one
// read per interval with every concurrent wait. A wait that starts on the
// store stays there, even if this instance comes to own every segment
// meanwhile, as right after Run claims them or after a failover: it still
// returns, only up to one PollInterval later than a wait in memory would.
// When ctx is done first, it returns a [*NotReachedError] carrying the
// segment statuses it last saw.
func (r *Runner) WaitUntil(ctx context.Context, position uint64) error {
	for {
		// Take the change signal before checking, so a change that lands
		// between the check and the wait still wakes us.
		r.mu.Lock()
		changed := r.changed
		statuses := make([]Status, len(r.segs))
		reached, allLocal := true, true
		for i, s := range r.segs {
			statuses[i] = s.status
			allLocal = allLocal && s.status.Owner == r.p.Owner
			reached = reached && s.status.Position >= position
		}
		r.mu.Unlock()

		// A segment another instance owns only changes in the store, which
		// also holds this instance's segments: each commit lands there
		// before it reaches memory.
		if !allLocal {
			return r.probe.WaitUntil(ctx, position)
		}
		if reached {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return &NotReachedError{Name: r.p.Name, Position: position, Statuses: statuses, Err: ctx.Err()}
		}
	}
}

// setStatus replaces seg's status after a commit or a status publish made
// under l. It does nothing when l is no longer seg's lease: a rebuild reset
// the projection after the call reached the store, and the position it
// reports was wiped.
func (r *Runner) setStatus(seg int, l Lease, s Status) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.segs[seg].lease != l {
		return
	}
	r.segs[seg].status = s
	r.signalChange()
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
