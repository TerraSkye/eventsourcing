package projection

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"
)

// shedGrace is how many heartbeats in a row must see the same members
// before an instance over its quota gives segments up. While instances
// start together the membership changes for a moment; waiting it out
// keeps segments from moving back and forth.
const shedGrace = 2

// errShed ends work on a segment this instance gives up to stay within its
// quota; see [Runner.shouldShed].
var errShed = errors.New("projection: segment shed to another instance")

// heartbeatInterval is how often the runner sends its heartbeat: often
// enough that its member row never expires while it runs, and at least
// once per poll so a quota change is seen as soon as a claim would be.
func (r *Runner) heartbeatInterval() time.Duration {
	return min(r.p.PollInterval, r.p.ClaimTTL/3)
}

// runMembership sends this instance's heartbeat until ctx is done.
func (r *Runner) runMembership(ctx context.Context) {
	t := time.NewTicker(r.heartbeatInterval())
	defer t.Stop()
	failing := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		err := r.heartbeat(ctx)
		switch {
		case err != nil && ctx.Err() == nil && !failing:
			r.p.Logger.WarnContext(ctx, "projection heartbeat failed; claiming no new segments",
				slog.String("projection", r.p.Name), slog.String("owner", r.p.Owner), slog.String("error", err.Error()))
			failing = true
		case err == nil && failing:
			r.p.Logger.InfoContext(ctx, "projection heartbeat recovered",
				slog.String("projection", r.p.Name), slog.String("owner", r.p.Owner))
			failing = false
		}
	}
}

// heartbeat records this instance with [Store.Heartbeat] and recomputes
// its quota from the members it returns. When the heartbeat fails, the
// instance keeps its quota but claims nothing new and sheds nothing, so a
// store outage can neither make it take every segment nor give them up.
func (r *Runner) heartbeat(ctx context.Context) error {
	members, err := r.store.Heartbeat(ctx, r.p.Name, r.p.Owner, r.p.Version, r.p.ClaimTTL)
	r.mu.Lock()
	if err != nil {
		r.membersOK = false
		r.stable = 0
		r.mu.Unlock()
		return err
	}
	if sig := membership(members); sig == r.membership {
		r.stable++
	} else {
		r.membership = sig
		r.stable = 0
	}
	r.quota = quota(members, r.p.Owner, r.segments)
	r.membersOK = true
	over := r.held > r.quota && r.stable >= shedGrace
	r.mu.Unlock()
	if over {
		// Wake the segment to shed if it waits at the head of the log.
		r.Nudge()
	}
	return nil
}

// leave removes this instance from the members, so the others take its
// share at once rather than after the heartbeat's ttl. Best effort: if it
// fails, the member expires after ClaimTTL.
func (r *Runner) leave(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	_, _ = r.store.Heartbeat(ctx, r.p.Name, r.p.Owner, r.p.Version, 0)
}

// membership identifies a set of members by owner and version, to tell
// whether it changed between two heartbeats.
func membership(members []Member) string {
	var b strings.Builder
	for _, m := range members {
		b.WriteString(m.Owner)
		b.WriteByte(0)
		b.WriteString(strconv.Itoa(m.Version))
		b.WriteByte(0)
	}
	return b.String()
}

// quota returns how many of segments owner should hold. Only members on
// the highest Version count: an older instance steps aside from segments a
// newer one rebuilt, so a quota of its own would leave them unclaimed. The
// quotas of the members that count sum to segments.
//
// Every member gets segments/n, and the members holding the most segments
// get one more each until the rest is handed out, so a segment only moves
// when the counts require it. Ties go by Owner, so every instance computes
// the same quotas from the same members.
func quota(members []Member, owner string, segments int) int {
	top := 0
	for _, m := range members {
		top = max(top, m.Version)
	}
	live := slices.DeleteFunc(slices.Clone(members), func(m Member) bool { return m.Version != top })
	slices.SortFunc(live, func(a, b Member) int {
		return cmp.Or(cmp.Compare(b.Segments, a.Segments), cmp.Compare(a.Owner, b.Owner))
	})
	i := slices.IndexFunc(live, func(m Member) bool { return m.Owner == owner })
	if i < 0 {
		return 0 // not (yet) a member, or on an older version: claim nothing
	}
	n := len(live)
	q := segments / n
	if i < segments%n {
		q++
	}
	return q
}

// reserve reports whether seg's loop may claim it: always while this
// instance holds it, otherwise only while the instance holds fewer
// segments than its quota and its last heartbeat succeeded. A granted
// reservation counts as held until [Runner.unreserve], so the instance's
// own loops cannot claim past the quota together.
func (r *Runner) reserve(seg int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.segs[seg]
	if s.reserved {
		return true
	}
	if !r.membersOK || r.held >= r.quota {
		return false
	}
	s.reserved = true
	r.held++
	return true
}

// unreserve gives seg's reservation back, once the instance no longer
// holds the segment or failed to claim it.
func (r *Runner) unreserve(seg int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.segs[seg]; s.reserved {
		s.reserved = false
		r.held--
	}
}

// shouldShed reports whether seg's loop should release the segment, as the
// instance holds more segments than its quota and the members have been
// stable for shedGrace heartbeats. The instance gives up its
// highest-numbered segment first and segment 0, which serves rebuild
// requests, last.
func (r *Runner) shouldShed(seg int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.membersOK || r.stable < shedGrace || r.held <= r.quota {
		return false
	}
	for i := len(r.segs) - 1; i > 0; i-- {
		if r.segs[i].reserved {
			return i == seg
		}
	}
	return seg == 0
}
