package projection

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrClaimLost is returned by [Store.Commit] and [Store.Reset] when the
	// owner no longer holds the segment: its claim expired and another
	// instance took over. Any writes the call made in its unit of work are
	// rolled back; the runner stops working on the segment and lets the new
	// owner continue.
	ErrClaimLost = errors.New("projection: claim lost")

	// ErrSegmentsChanged is returned by [Store.Init] when the projection
	// exists with a different segment count. Events would map to different
	// segments than the stored checkpoints were written for, so changing the
	// count takes a new projection name.
	ErrSegmentsChanged = errors.New("projection: segment count differs from the stored one")
)

// Checkpoint records how far a segment of a projection has processed the
// log, and which projection version wrote the read model.
type Checkpoint struct {
	// Position is the GlobalVersion up to which the segment has processed
	// the log. Zero means nothing has been processed yet.
	Position uint64

	// Version is the projection version that wrote the read model; see
	// [Projection.Version].
	Version int

	// ReplayUntil is the position up to which events count as replayed; see
	// [IsReplaying]. It is set when a segment first starts (to the head of
	// the log at that moment) and on every rebuild (to the position the
	// segment had reached before it).
	ReplayUntil uint64
}

// Phase is where a segment of a projection is in its life cycle.
type Phase int

const (
	// Starting means [Runner.Run] has not yet claimed and loaded this
	// segment.
	Starting Phase = iota

	// Standby means another instance holds the segment; this runner waits
	// to take over if that instance's claim expires.
	Standby

	// CatchingUp means the segment is behind the head of the log and
	// reading full batches: on a fresh start, during a rebuild, or after
	// falling behind.
	CatchingUp

	// Live means the last read reached the head of the log. The read model
	// reflects every event the segment handles that was appended up to that
	// read.
	Live

	// Stalled means the last attempt failed; the runner is backing off
	// before it retries. [Status.LastError] says why.
	Stalled
)

// String returns the phase's lower-case name, as stored by [Store].
func (p Phase) String() string {
	switch p {
	case Starting:
		return "starting"
	case Standby:
		return "standby"
	case CatchingUp:
		return "catching_up"
	case Live:
		return "live"
	case Stalled:
		return "stalled"
	default:
		return "unknown"
	}
}

// Status describes how a projection, or one of its segments, is doing. The
// runner publishes it to the [Store] after every committed batch and on
// every change of [Phase]; other instances read it with [Store.LoadStatuses]
// to answer [Runner.Live] and [Runner.WaitUntil] about segments they do
// not own.
type Status struct {
	// Phase is where the segment is in its life cycle.
	Phase Phase

	// Position is the checkpoint: the GlobalVersion up to which the segment
	// has processed the log.
	Position uint64

	// Head is the head of the log as of the last read; Head minus Position
	// is how far the segment lags behind.
	Head uint64

	// Version is the projection version the read model was built with.
	Version int

	// LastError is the error that stalled the segment, or "" when it is not
	// [Stalled].
	LastError string

	// Owner identifies the instance running the segment. "" when no
	// instance currently holds it.
	Owner string

	// Segment is which segment this status describes. 0 for an
	// unsegmented projection ([Projection.Segments] <= 1).
	Segment int

	// Age is how long ago this status was published, by the store's clock
	// (so clock skew between machines does not matter). It is only set on a
	// status returned by [Store.LoadStatuses].
	Age time.Duration
}

// Store is everything a projection persists: segment claims, checkpoints,
// and status. One Store serves every projection whose read models live in
// its database, and must sit in that same database so that read-model
// writes and checkpoint moves commit together.
//
// The Store absorbs five separate concerns, so users wire up one thing per
// database instead of five:
//
//   - checkpointing (via Commit and Load)
//   - exclusive ownership between instances (via Claim and the fenced
//     Commit; no separate lock service)
//   - segmentation across instances (segment-keyed Claim; a single
//     projection is just segments=1)
//   - exactly-once writes to the read model (Commit opens the unit of work
//     and refuses to commit if the claim has been taken over)
//   - status publication for other instances to follow (SaveStatus /
//     LoadStatuses, stored next to the checkpoint)
//
// Implementations are checked against the contract by
// [projectiontest.StoreAcceptanceTest].
//
// # Contract
//
// Init declares the segments. Every other call for name assumes Init has
// been called for it. Calling Init with the same segments is a no-op; with
// a different count it returns [ErrSegmentsChanged].
//
// A segment has at most one owner at a time. Claim succeeds when the
// segment is unowned, is already owned by the caller, or its last recorded
// heartbeat is older than ttl (so a dead instance's claim is reclaimed).
// On success it refreshes the heartbeat and returns the segment's current
// Checkpoint.
//
// Commit opens one unit of work: it runs fn, and if fn returns nil it
// stores cp and status and refreshes the owner's heartbeat, atomically.
// If the owner no longer holds the segment (claim expired, Release was
// called elsewhere, or a reclaim happened), the whole unit of work rolls
// back and Commit returns [ErrClaimLost]. The ctx passed to fn carries the
// unit of work; each implementation provides its own accessor, such as
// postgres.Tx in projection/postgres.
//
// Reset wipes the read model via wipe, and in the same unit of work resets
// the segment's checkpoint to Position=0, Version=newVersion and
// ReplayUntil=currentPosition. It is fenced the same way as Commit.
//
// Release gives the segment up at once instead of waiting for its claim's
// ttl to expire, so another instance can take over without delay.
//
// SaveStatus publishes status for a segment the caller still owns, so that
// other instances see phase changes between batches (e.g. Standby -> Live)
// without waiting for the next Commit. It stores Phase, Head and LastError
// only; the Position and Version that LoadStatuses reports always come
// from the stored checkpoint, and Owner from the claim. A call by anyone
// other than the owner changes nothing. It is best-effort; a failure does
// not stall the runner.
type Store interface {
	Init(ctx context.Context, name string, segments int) error

	Claim(ctx context.Context, name string, segment int, owner string, ttl time.Duration) (Checkpoint, bool, error)

	Commit(ctx context.Context, name string, segment int, owner string, cp Checkpoint, status Status,
		fn func(ctx context.Context) error) error

	Reset(ctx context.Context, name string, segment int, owner string, newVersion int,
		wipe func(ctx context.Context) error) error

	Release(ctx context.Context, name string, segment int, owner string) error

	SaveStatus(ctx context.Context, name string, segment int, owner string, s Status) error

	LoadStatuses(ctx context.Context, name string) ([]Status, error)
}

// DeadLetterStore is optionally implemented by a [Store] to record events
// the runner skipped under a non-stalling error policy, so they can be
// inspected and replayed. It is called inside the Commit that moves the
// checkpoint past the event, so the record and the move commit together.
type DeadLetterStore interface {
	// SaveDeadLetter records that name/segment skipped the event at
	// position because its handler returned cause. ctx carries the Commit's
	// unit of work.
	SaveDeadLetter(ctx context.Context, name string, segment int, position uint64, eventType string, cause error) error
}
