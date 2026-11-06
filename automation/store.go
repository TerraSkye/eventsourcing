package automation

import (
	"context"
	"time"
)

// Store keeps the segment leases of the default scheduler: which instance
// holds each key, until when, and when a key may run again. The memory,
// sqlite and postgres packages each provide one as AutomationStore: the
// memory one for an automation that runs in one process, the SQL ones for
// instances that share a database and divide segments between them.
// automationtest.StoreAcceptanceTest checks an implementation against
// this contract.
//
// Its methods and their signatures may change in minor releases before
// v1.0; external implementations should expect breakage.
type Store interface {
	// Acquire records candidates it has not seen, then grants up to max
	// eligible keys to owner for ttl. A key is eligible when nobody holds
	// a live lease on it and its pause has passed. Candidates are ordered
	// by the group that has waited longest first, then by the key that
	// has waited longest within it; keys with no group form one group.
	// Every grant raises the key's epoch. max may be 0: the call still
	// records candidates and reports Active and Eligible.
	Acquire(ctx context.Context, name, owner string, candidates []Segment, max int, ttl time.Duration) (AcquireResult, error)

	// Renew extends every lease in held by ttl and returns the ones the
	// owner no longer holds.
	Renew(ctx context.Context, held []SegmentLease, ttl time.Duration) (lost []SegmentLease, err error)

	// Release gives segments back, each fenced on owner and epoch. A
	// released key is paused for DeferFor (0 for none) and, when Served,
	// moves to the back of the fairness order. It returns the leases that
	// were lost; nothing changes for those.
	Release(ctx context.Context, rel []ReleaseRequest) (lost []SegmentLease, err error)

	// Defer pauses key for d from now, keeping any longer pause already
	// set. It is not fenced: a stale writer can only delay the next grant,
	// never disturb a holder. key is "" for an unsegmented automation.
	Defer(ctx context.Context, name, key string, d time.Duration) error

	// DeferredFor returns how long key is still paused, or 0.
	DeferredFor(ctx context.Context, name, key string) (time.Duration, error)
}

// SegmentLease identifies one grant of a segment to an owner. Epoch fences
// it: a later grant of the same key has a higher epoch, and operations
// carrying an older one are refused.
type SegmentLease struct {
	Name, Key, Owner string
	Epoch            uint64
}

// ReleaseRequest is one segment to give back; see [Store.Release].
type ReleaseRequest struct {
	Lease    SegmentLease
	DeferFor time.Duration
	Served   bool
}

// AcquireResult is what [Store.Acquire] granted and what it saw.
type AcquireResult struct {
	// Granted are the new leases, in grant order.
	Granted []SegmentLease

	// Active counts candidates whose pause has passed, held or not.
	Active int

	// Eligible counts candidates that nobody holds and whose pause has
	// passed, after this grant: keys that are waiting for an owner.
	Eligible int
}
