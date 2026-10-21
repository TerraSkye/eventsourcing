package projection

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultProbeInterval is how often a [Probe] reads the store while a
	// wait is in progress, unless [WithPollInterval] says otherwise.
	DefaultProbeInterval = 100 * time.Millisecond

	// probeReadTimeout bounds one read of the store by a Probe's poller, so
	// a hung connection delays waiters instead of stopping every later
	// read.
	probeReadTimeout = 5 * time.Second
)

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
// Concurrent waits share one read of the store per poll interval, however
// many there are, so create one Probe per projection per process and share
// it, rather than one per request. The poller runs only while a wait is in
// progress, so a Probe needs no closing.
//
// A Probe is safe for concurrent use.
type Probe struct {
	store    Store
	name     string
	interval time.Duration
	claimTTL time.Duration

	// segments is how many segments to expect, or 0 when unknown. A Runner
	// knows its count; a standalone Probe takes whatever the store holds.
	segments int

	mu       sync.Mutex
	waiters  int
	stop     context.CancelFunc // stops the running poller; nil when none runs
	statuses []Status           // from the poller's latest successful read
	storeErr error              // the poller's latest read's error, if it failed
	changed  chan struct{}      // closed and replaced after every read
}

// ProbeOption configures a [Probe].
type ProbeOption func(*Probe)

// WithPollInterval sets how often the Probe reads the store while a wait is
// in progress. Default [DefaultProbeInterval].
func WithPollInterval(d time.Duration) ProbeOption {
	return func(p *Probe) { p.interval = d }
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
	p := &Probe{store: store, name: name, changed: make(chan struct{})}
	for _, opt := range opts {
		opt(p)
	}
	if p.interval <= 0 {
		p.interval = DefaultProbeInterval
	}
	if p.claimTTL <= 0 {
		p.claimTTL = DefaultClaimTTL
	}
	return p
}

// Name returns the projection's name.
func (p *Probe) Name() string { return p.name }

// IsLive reports whether the projection is live: every segment caught up
// with the head of the log and held by a runner that is still publishing,
// as judged by [Status.IsLive]. A projection no runner has started yet is
// not live. It reads the store on every call.
func (p *Probe) IsLive(ctx context.Context) (bool, error) {
	statuses, err := p.store.LoadStatuses(ctx, p.name)
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
// including position on every segment, or until ctx is done.
//
// position is an [eventsourcing.Envelope.GlobalPosition], typically from the
// [eventsourcing.AppendResult] of the command whose effects the caller
// wants to see. Events the projection does not handle count as processed.
// A projection no runner has started yet has not reached any position.
//
// WaitUntil returns nil once the position is reached. Otherwise only ctx
// ends the wait: it returns a [*NotReachedError] carrying the statuses it
// last saw, so the caller can tell a stalled projection from a slow one.
// A failing store does not end the wait; the error of the latest failed
// read is in [NotReachedError.StoreErr].
//
// A rebuild moves the projection's position back to 0 and wipes its read
// model, so a wait that returns just before a rebuild starts may be
// followed by reads that miss the write until the rebuild catches up.
func (p *Probe) WaitUntil(ctx context.Context, position uint64) error {
	// The first wait starts the poller and the last one stops it.
	p.mu.Lock()
	if p.waiters++; p.waiters == 1 {
		pollCtx, cancel := context.WithCancel(context.Background())
		p.stop = cancel
		go p.poll(pollCtx)
	}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		if p.waiters--; p.waiters == 0 {
			p.stop()
			p.stop = nil
		}
		p.mu.Unlock()
	}()

	for {
		p.mu.Lock()
		statuses, storeErr, changed := p.statuses, p.storeErr, p.changed
		p.mu.Unlock()
		if p.reached(statuses, position) {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return &NotReachedError{Name: p.name, Position: position, Statuses: statuses, StoreErr: storeErr, Err: ctx.Err()}
		}
	}
}

// poll reads the store at once and then every interval until ctx is done,
// publishing each read to the waiters.
//
// A waiter that joins while poll runs waits up to one interval for the
// next read rather than starting one of its own, so the reads follow the
// interval and not the number of waiters.
func (p *Probe) poll(ctx context.Context) {
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		readCtx, cancel := context.WithTimeout(ctx, probeReadTimeout)
		statuses, err := p.store.LoadStatuses(readCtx, p.name)
		cancel()

		p.mu.Lock()
		// Stopped mid-read: a poller started since owns the results. The
		// last wait stops a poller under p.mu, so this check is not racy.
		if ctx.Err() != nil {
			p.mu.Unlock()
			return
		}
		if err == nil {
			p.statuses = statuses
		}
		p.storeErr = err
		close(p.changed)
		p.changed = make(chan struct{})
		p.mu.Unlock()

		t.Reset(p.interval)
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

// NotReachedError is returned by [Probe.WaitUntil] and [Runner.WaitUntil]
// when the context ends the wait before the projection reached the
// position.
type NotReachedError struct {
	// Name is the projection's name.
	Name string

	// Position is the position waited for.
	Position uint64

	// Statuses are the segment statuses last seen, ordered by segment;
	// empty when no runner had started the projection. [NotReachedError.Behind]
	// picks the one that held the wait up.
	Statuses []Status

	// StoreErr is the error of the latest read of the store, when that read
	// failed; nil otherwise. Statuses are then from the last read that
	// succeeded.
	StoreErr error

	// Err is the context's error.
	Err error
}

// Error describes the segment that held the wait up, and why when it is
// stalled.
func (e *NotReachedError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "projection %q: position %d not reached", e.Name, e.Position)
	switch behind := e.Behind(); {
	case len(e.Statuses) == 0:
		b.WriteString(": no status published")
	case behind == nil:
	case behind.Phase == Stalled:
		fmt.Fprintf(&b, ": segment %d stalled at %d: %s", behind.Segment, behind.Position, behind.LastError)
	default:
		fmt.Fprintf(&b, ": segment %d %s at %d", behind.Segment, behind.Phase, behind.Position)
	}
	if e.StoreErr != nil {
		fmt.Fprintf(&b, ": reading status: %v", e.StoreErr)
	}
	if e.Err != nil {
		fmt.Fprintf(&b, ": %v", e.Err)
	}
	return b.String()
}

// Behind returns the segment that held the wait up: the first one behind
// Position, preferring a [Stalled] one, or nil if none is behind. A caller
// can check its Phase to tell a stalled projection, which may need
// attention, from a slow one worth retrying.
func (e *NotReachedError) Behind() *Status {
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

// Unwrap returns Err and StoreErr, so errors.Is matches either the
// context's error or the store's.
func (e *NotReachedError) Unwrap() []error {
	var errs []error
	for _, err := range []error{e.Err, e.StoreErr} {
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}
