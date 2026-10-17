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

	// ErrSegmentsChanged is returned by [Store.Declare] when the projection
	// exists with a different segment count. Events would map to different
	// segments than the stored checkpoints were written for, so changing the
	// count takes a new projection name.
	ErrSegmentsChanged = errors.New("projection: segment count differs from the stored one")
)

// Checkpoint records how far a segment of a projection has processed the
// log, and which projection version wrote the read model.
type Checkpoint struct {
	// Position is the GlobalPosition up to which the segment has processed
	// the log. Zero means nothing has been processed yet.
	Position uint64

	// Version is the projection version that wrote the read model; see
	// [Definition.Version].
	Version int

	// ReplayUntil is the position up to which events count as replayed; see
	// [IsReplaying]. It is set when [Store.Declare] creates the segment (to the
	// head of the log at that moment) and on every rebuild (to the position
	// the segment had reached before it).
	ReplayUntil uint64

	// Fingerprint identifies the [Definition.EventTypes] the checkpoint was
	// written for. Empty means unknown: a segment that has not committed
	// since it was created or reset.
	Fingerprint string
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
// to answer [Runner.IsLive] and [Runner.WaitUntil] about segments they do
// not own.
type Status struct {
	// Phase is where the segment is in its life cycle.
	Phase Phase

	// Position is the checkpoint: the GlobalPosition up to which the segment
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
	// unsegmented projection ([Definition.Segments] <= 1).
	Segment int

	// Age is how long ago this status was published, by the store's clock
	// (so clock skew between machines does not matter). It is only set on a
	// status returned by [Store.LoadStatuses].
	Age time.Duration
}

// Lease identifies one segment of a projection and the instance that
// claims it. Every [Store] call that acts on a claimed segment takes one.
type Lease struct {
	// Name is the projection's name; see [Definition.Name].
	Name string

	// Segment is the segment, in [0, segments). 0 for a single-segment
	// projection.
	Segment int

	// Owner identifies the instance that claims the segment; see
	// [Definition.Owner].
	Owner string

	// Epoch is the fencing token of the claim, as returned by
	// [Store.Claim]. Every takeover of a segment gives it a higher epoch,
	// so a call made under an earlier claim is refused even when it comes
	// from the same Owner: a process restarted under a fixed Owner, or one
	// that stalled past its claim's ttl and resumed. Zero before the first
	// claim.
	Epoch uint64
}

// Store is everything a projection persists: segment claims, checkpoints,
// and status. One Store serves every projection whose read models live in
// its database, and must sit in that same database so that read-model
// writes and checkpoint moves commit together.
//
// The Store absorbs five separate concerns, so users wire up one thing per
// database instead of five:
//
//   - checkpointing (via Claim and Commit)
//   - exclusive ownership between instances (via Claim and the fenced
//     Commit; no separate lock service)
//   - segmentation across instances (a [Lease] names one segment; a single
//     projection is just segments=1)
//   - exactly-once writes to the read model (Commit opens the unit of work
//     and refuses to commit if the claim has been taken over)
//   - status publication for other instances to follow (SaveStatus and
//     LoadStatuses, stored next to the checkpoint)
//
// A segment has at most one owner at a time. The calls that take a [Lease]
// are fenced: when l.Owner no longer holds the segment, or l.Epoch is not
// the epoch of its current claim, they change nothing, and Commit, Reset
// and SaveStatus return [ErrClaimLost].
//
// Implementations must be safe for concurrent use, and are checked against
// this contract by [projectiontest.StoreAcceptanceTest].
type Store interface {
	// Declare states that the projection name has segments segments,
	// creating them on first use with a zero checkpoint whose ReplayUntil is
	// replayUntil. Every other call for name assumes Declare has been called
	// for it. Calling Declare again with the same count changes nothing, not
	// even ReplayUntil; with a different count it returns
	// [ErrSegmentsChanged].
	Declare(ctx context.Context, name string, segments int, replayUntil uint64) error

	// Claim takes the segment for l.Owner. When l is the segment's current
	// claim (same Owner and Epoch), Claim refreshes it and keeps its epoch.
	// Otherwise it takes the segment over when it is unowned or its last
	// heartbeat is older than ttl (so a dead instance's claim is reclaimed),
	// giving the claim a higher epoch than any before it. On success it
	// refreshes the heartbeat and returns the claim's Lease, the segment's
	// checkpoint and true; pass that Lease to every later call. When another
	// claim holds the segment it returns false.
	Claim(ctx context.Context, l Lease, ttl time.Duration) (Lease, Checkpoint, bool, error)

	// Commit runs fn as one unit of work and, if fn returns nil, stores cp
	// and status and refreshes the heartbeat in that same unit of work. If
	// fn fails, or l is no longer the segment's current claim, everything
	// fn wrote is rolled back. The ctx passed to fn carries the unit of
	// work; each implementation provides its own accessor, such as
	// postgres.Tx.
	Commit(ctx context.Context, l Lease, cp Checkpoint, status Status, fn func(ctx context.Context) error) error

	// Reset runs wipe to empty the read model and, in the same unit of
	// work, moves the segment's checkpoint back to Position 0 with
	// Version newVersion, ReplayUntil set to the position it had reached,
	// and an empty Fingerprint. It is fenced like Commit.
	Reset(ctx context.Context, l Lease, newVersion int, wipe func(ctx context.Context) error) error

	// Release gives the segment up at once instead of waiting for its
	// claim's ttl to expire, so another instance can take over without
	// delay, and sets its phase to [Standby] so no one reads it as live. It
	// does nothing when l is not the segment's current claim.
	Release(ctx context.Context, l Lease) error

	// SaveStatus publishes status between commits, so other instances see
	// phase changes (Standby to Live, for example) without waiting for the
	// next Commit, and refreshes the heartbeat. It stores Phase, Head and
	// LastError only: the Position and Version that LoadStatuses reports
	// come from the checkpoint, and Owner from the claim. When l is not the
	// segment's current claim it changes nothing and returns [ErrClaimLost].
	SaveStatus(ctx context.Context, l Lease, status Status) error

	// LoadStatuses returns the status of every segment of the projection
	// name, ordered by segment, with Age set.
	LoadStatuses(ctx context.Context, name string) ([]Status, error)
}

// DeadLetterStore is optionally implemented by a [Store] to record events
// the runner skipped under a non-stalling error policy, so they can be
// inspected and replayed. It is called inside the Commit that moves the
// checkpoint past the event, so the record and the move commit together.
type DeadLetterStore interface {
	// SaveDeadLetter records that the segment l skipped the event at
	// position because its handler returned cause. ctx carries the Commit's
	// unit of work.
	SaveDeadLetter(ctx context.Context, l Lease, position uint64, eventType string, cause error) error
}
