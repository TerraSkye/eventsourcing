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
	// head of the log at that moment) and raised on every rebuild (to the
	// position the segment had reached before it, if that is higher).
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

	// ReplayUntil is the position up to which the segment is replaying
	// events it processed before: after a rebuild, the position it had
	// reached; after it was first declared, the head of the log then. While
	// Position is below it, the segment is replaying rather than catching up
	// on new events. Only set on a status returned by [Store.LoadStatuses].
	ReplayUntil uint64

	// ClaimTTL is the ttl the owner took its claim with: the claim expires
	// once Age passes it. 0 when no instance holds the segment. Only set
	// on a status returned by [Store.LoadStatuses].
	ClaimTTL time.Duration

	// RebuildRequested reports that a rebuild of the projection was
	// requested with [Store.RequestRebuild] and has not run yet. It is the
	// same on every segment. Only set on a status returned by
	// [Store.LoadStatuses].
	RebuildRequested bool
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
//     projection is just segments=1; Heartbeat tells each instance how
//     many others share the segments)
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

	// Claim takes the segment for l.Owner for ttl. When l is the segment's
	// current claim (same Owner and Epoch), Claim refreshes it and keeps its
	// epoch. Otherwise it takes the segment over when it is unowned or its
	// last heartbeat is older than the ttl its current claim was taken with
	// (so a dead instance's claim is reclaimed), giving the claim a higher
	// epoch than any before it. The claim's ttl is the holder's, not the
	// caller's: instances configured with different ClaimTTLs judge a claim
	// alike. On success it
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

	// Reset rebuilds the whole projection l.Name, every segment at once,
	// as the read model is one and wipe empties all of it. In one unit of
	// work it runs wipe, moves every segment's checkpoint back to
	// Position 0 with Version newVersion, ReplayUntil set to the larger of
	// its current value and the position that segment had reached, and an
	// empty Fingerprint, and marks every
	// rebuild requested so far with RequestRebuild as done. It releases every
	// segment's claim except l's, so an instance still working one gets
	// [ErrClaimLost] on its next Commit instead of writing into the wiped
	// read model, and claims it again at the new checkpoint. A store that
	// implements [DeadLetterStore] also deletes every dead letter of the
	// projection in that unit of work: the replay runs each event through
	// the handler again, and records the ones that still fail afresh.
	// Reset waits for Commits already in progress to finish, so none of
	// their writes lands after wipe. It is fenced on l like Commit.
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
	// name, ordered by segment, with Age, ClaimTTL, ReplayUntil and
	// RebuildRequested set.
	LoadStatuses(ctx context.Context, name string) ([]Status, error)

	// RequestRebuild asks for the projection name to be rebuilt, wherever
	// it runs: the runner holding segment 0 sees the request and rebuilds
	// through Reset, which marks it done. The request is kept in the store,
	// so it survives restarts and claims moving between instances, and
	// requests made before a rebuild runs are all served by it. It returns
	// the request's number, counting up from 1 per projection, and
	// [ErrUnknownProjection] when no runner has declared name.
	RequestRebuild(ctx context.Context, name string) (uint64, error)

	// Rebuilds returns the number of the last rebuild requested for the
	// projection name and of the last one Reset marked done. requested >
	// done means a rebuild is pending. Both are 0 for a projection never
	// asked to rebuild, or not declared.
	Rebuilds(ctx context.Context, name string) (requested, done uint64, err error)

	// Heartbeat records owner as a live instance of the projection name
	// running version until ttl passes without another heartbeat, and
	// returns the instances whose own ttl has not passed, owner included, ordered by Owner, with Age measured by the store's clock
	// and Segments counting the claims each one holds on name. A ttl of 0
	// removes owner instead, for a clean shutdown, and returns no instances.
	// The runner calls it to spread the segments over its instances.
	Heartbeat(ctx context.Context, name, owner string, version int, ttl time.Duration) ([]Instance, error)
}

// Instance is a runner of a projection that sent a heartbeat within the
// ttl, as returned by [Store.Heartbeat].
type Instance struct {
	// Owner identifies the instance; see [Definition.Owner].
	Owner string

	// Version is the [Definition.Version] the instance runs.
	Version int

	// Segments is the number of segments whose claim the instance holds.
	Segments int

	// Age is how long ago the instance last sent a heartbeat, by the
	// store's clock.
	Age time.Duration
}

// DeadLetter is an event a segment skipped instead of applying.
type DeadLetter struct {
	// Position is the event's GlobalPosition.
	Position uint64

	// StreamID is the event's stream. A stream with a dead letter is
	// parked: the segment skips its later events too, until
	// [Runner.ReplayStream] applies them.
	StreamID string

	// EventType is the event's registered name.
	EventType string

	// Error is why the event was skipped: the handler's error, or
	// [ErrStreamParked] for a later event of a parked stream.
	Error string
}

// DeadLetterStore is optionally implemented by a [Store] to record events
// the runner skipped, so they can be inspected and replayed. Without it a
// skipped event is gone, and a parked stream is forgotten when the runner
// stops.
type DeadLetterStore interface {
	// SaveDeadLetter records d for the segment l. It is called inside the
	// Commit that moves the checkpoint past the event, and ctx carries that
	// Commit's unit of work, so the record and the move commit together.
	SaveDeadLetter(ctx context.Context, l Lease, d DeadLetter) error

	// LoadDeadLetters returns the dead letters of segment segment of the
	// projection name, ordered by Position.
	LoadDeadLetters(ctx context.Context, name string, segment int) ([]DeadLetter, error)

	// DeleteDeadLetter removes the dead letter at position from the
	// segment l. It is called inside the Commit that applies the event
	// again, and ctx carries that Commit's unit of work.
	DeleteDeadLetter(ctx context.Context, l Lease, position uint64) error
}
