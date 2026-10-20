package projection

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
)

// DefaultProbeInterval is how often [Probe.WaitUntil] polls the store,
// unless [WithPollBackoff] says otherwise.
const DefaultProbeInterval = 100 * time.Millisecond

// Probe follows a projection through the status its runners publish to a
// [Store], without running it. It needs only the store and the
// projection's name: no handler, reader or registered event types.
//
// Use it where a projection is read but not run, such as an API answering
// queries from a read model a worker process keeps up to date: wait with
// [Probe.WaitUntil] until a write has reached the read model, or gate
// readiness on [Probe.IsLive]. A [Runner] uses a Probe for the segments
// another instance owns.
//
// A Probe is safe for concurrent use.
type Probe struct {
	store      Store
	name       string
	newBackoff func() backoff.BackOff
	claimTTL   time.Duration

	// segments is how many segments to expect, or 0 when unknown. A Runner
	// knows its count; a standalone Probe takes whatever the store holds.
	segments int
}

// ProbeOption configures a [Probe].
type ProbeOption func(*Probe)

// WithPollBackoff sets how long [Probe.WaitUntil] waits between polls of
// the store. newBackoff is called once per WaitUntil, as a
// [backoff.BackOff] keeps state. When the BackOff returns [backoff.Stop],
// WaitUntil gives up with a [*NotReachedError], which bounds a wait
// without a deadline on its context. Default a constant
// [DefaultProbeInterval].
func WithPollBackoff(newBackoff func() backoff.BackOff) ProbeOption {
	return func(p *Probe) { p.newBackoff = newBackoff }
}

// WithClaimTTL sets the [Definition.ClaimTTL] the projection runs with, so
// [Probe.IsLive] can tell a segment whose runner stopped from a live one.
// Default [DefaultClaimTTL].
func WithClaimTTL(ttl time.Duration) ProbeOption {
	return func(p *Probe) { p.claimTTL = ttl }
}

// NewProbe returns a [Probe] for the projection called name, as published
// to store.
func NewProbe(store Store, name string, opts ...ProbeOption) *Probe {
	p := &Probe{store: store, name: name}
	for _, opt := range opts {
		opt(p)
	}
	if p.newBackoff == nil {
		p.newBackoff = func() backoff.BackOff { return backoff.NewConstantBackOff(DefaultProbeInterval) }
	}
	if p.claimTTL <= 0 {
		p.claimTTL = DefaultClaimTTL
	}
	return p
}

// Name returns the projection's name.
func (p *Probe) Name() string { return p.name }

// Statuses returns the published status of every segment of the
// projection, ordered by segment; see [Store.LoadStatuses]. It is empty
// before a runner has started the projection.
func (p *Probe) Statuses(ctx context.Context) ([]Status, error) {
	return p.store.LoadStatuses(ctx, p.name)
}

// IsLive reports whether the projection is live: every segment caught up
// with the head of the log and held by a runner that is still publishing,
// as judged by [Status.IsLive]. A projection no runner has started yet is
// not live.
func (p *Probe) IsLive(ctx context.Context) (bool, error) {
	statuses, err := p.Statuses(ctx)
	if err != nil {
		return false, err
	}
	if !p.complete(statuses) {
		return false, nil
	}
	for _, s := range statuses {
		if !s.IsLive(p.claimTTL) {
			return false, nil
		}
	}
	return true, nil
}

// WaitUntil blocks until the projection has processed the log up to and
// including position on every segment.
//
// position is an [eventsourcing.Envelope.GlobalPosition], typically from the
// [eventsourcing.AppendResult] of the command whose effects the caller
// wants to see. Events the projection does not handle count as processed.
// A projection no runner has started yet has not reached any position.
//
// WaitUntil polls the store as [WithPollBackoff] sets. When ctx is done or
// the backoff stops first, it returns a [*NotReachedError] carrying the
// statuses it last saw, so the caller can tell a stalled projection from a
// slow one. An error from the store ends the wait at once.
func (p *Probe) WaitUntil(ctx context.Context, position uint64) error {

	p.newBackoff().NextBackOff()
	bo := p.newBackoff()
	for {
		statuses, err := p.Statuses(ctx)
		if err != nil {
			return err
		}
		if p.reached(statuses, position) {
			return nil
		}
		wait := bo.NextBackOff()
		if wait == backoff.Stop {
			return p.notReached(position, statuses, nil)
		}
		sleepCtx(ctx, wait)
		if err := ctx.Err(); err != nil {
			return p.notReached(position, statuses, err)
		}
	}
}

// complete reports whether statuses covers every segment of the
// projection.
func (p *Probe) complete(statuses []Status) bool {
	return len(statuses) > 0 && len(statuses) >= p.segments
}

// reached reports whether every segment in statuses has processed the log
// up to position.
func (p *Probe) reached(statuses []Status, position uint64) bool {
	if !p.complete(statuses) {
		return false
	}
	for _, s := range statuses {
		if s.Position < position {
			return false
		}
	}
	return true
}

func (p *Probe) notReached(position uint64, statuses []Status, err error) *NotReachedError {
	return &NotReachedError{Name: p.name, Position: position, Statuses: statuses, Err: err}
}

// NotReachedError is returned by [Probe.WaitUntil] and [Runner.WaitUntil]
// when the wait ends before the projection reached the position.
type NotReachedError struct {
	// Name is the projection's name.
	Name string

	// Position is the position waited for.
	Position uint64

	// Statuses are the segment statuses last seen, ordered by segment;
	// empty when no runner had started the projection. A segment behind
	// Position in [Stalled] says why in its LastError.
	Statuses []Status

	// Err is the context's error when the context ended the wait, or nil
	// when the poll backoff stopped.
	Err error
}

// Error describes the first segment that is behind, and why when it is
// stalled.
func (e *NotReachedError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "projection %q: position %d not reached", e.Name, e.Position)
	switch behind := e.behind(); {
	case len(e.Statuses) == 0:
		b.WriteString(": no status published")
	case behind == nil:
	case behind.Phase == Stalled:
		fmt.Fprintf(&b, ": segment %d stalled at %d: %s", behind.Segment, behind.Position, behind.LastError)
	default:
		fmt.Fprintf(&b, ": segment %d %s at %d", behind.Segment, behind.Phase, behind.Position)
	}
	if e.Err != nil {
		fmt.Fprintf(&b, ": %v", e.Err)
	}
	return b.String()
}

// behind returns the first segment that has not reached Position,
// preferring a stalled one, or nil if there is none.
func (e *NotReachedError) behind() *Status {
	var first *Status
	for i := range e.Statuses {
		s := &e.Statuses[i]
		if s.Position >= e.Position {
			continue
		}
		if s.Phase == Stalled {
			return s
		}
		if first == nil {
			first = s
		}
	}
	return first
}

// Unwrap returns Err, so errors.Is(err, context.DeadlineExceeded) still
// holds for a wait its context's deadline ended.
func (e *NotReachedError) Unwrap() error { return e.Err }
