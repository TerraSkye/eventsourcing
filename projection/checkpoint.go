package projection

import (
	"context"

	"github.com/terraskye/eventsourcing"
)

// Checkpoint records how far a projection has processed the log, and which
// projector version built its read model.
type Checkpoint struct {
	// Position is the GlobalVersion up to which the log has been processed.
	// Zero means nothing has been processed yet.
	Position uint64

	// Version is the projector version that wrote the read model; see
	// [WithVersion].
	Version int

	// ReplayUntil is the position up to which events count as replayed;
	// see [IsReplaying]. It is set when a projection first starts (to the
	// head of the log at that moment) and on every rebuild (to the
	// position the projection had reached before it).
	ReplayUntil uint64
}

// CheckpointStore persists a projection's [Checkpoint], atomically with the
// writes its handlers make.
//
// An implementation belongs to one database, the one holding the read
// models whose checkpoints it stores. It may additionally implement
// [StatusStore], [Locker] and [DeadLetterStore], which the runner detects
// and uses.
type CheckpointStore interface {
	// Load returns the stored checkpoint for the projection called name, or
	// the zero Checkpoint if there is none.
	Load(ctx context.Context, name string) (Checkpoint, error)

	// Commit runs fn inside a new unit of work and, if fn succeeds, stores
	// cp for name in that same unit of work and commits it. If fn or the
	// store fails, nothing fn did is kept and the stored checkpoint is
	// unchanged.
	//
	// The ctx passed to fn carries the unit of work, so that handlers
	// called by fn can write through it; each implementation provides its
	// own accessor, such as postgres.Tx in projection/postgres.
	//
	// An implementation that cannot roll back fn's writes (an in-memory
	// one, say) must still store cp only after fn succeeds. Projections
	// using it then get at-least-once delivery instead of exactly-once.
	Commit(ctx context.Context, name string, cp Checkpoint, fn func(ctx context.Context) error) error
}

// StatusStore is optionally implemented by a [CheckpointStore] to publish
// each projection's [Status] where other processes can read it, next to the
// checkpoint.
//
// With a StatusStore, [Runner.Live] and [Runner.WaitUntil] work on every
// instance, not only the active one, and [Remote] can follow the
// projection from another program.
type StatusStore interface {
	// SaveStatus stores s as the current status of the projection called
	// name, and records when, by the store's own clock.
	//
	// The runner calls it on every change of [Phase], and on every
	// heartbeat when [WithHeartbeat] is set; status changes are written
	// immediately, outside any batch's unit of work.
	SaveStatus(ctx context.Context, name string, s Status) error

	// LoadStatus returns the last status saved for name, with Age set to
	// how long ago it was saved. found is false if no status was ever
	// saved for name.
	LoadStatus(ctx context.Context, name string) (s Status, found bool, err error)
}

// Locker is optionally implemented by a [CheckpointStore] to make sure each
// projection runs on exactly one instance at a time.
//
// Every runner of a projection calls TryLock with the projection's name.
// The one that acquires the lock becomes active; the others report
// [Standby] and try again periodically.
type Locker interface {
	// TryLock tries to acquire the lock for the projection called name
	// without waiting. acquired is false, with a nil error, when another
	// instance holds it.
	TryLock(ctx context.Context, name string) (lock Lock, acquired bool, err error)
}

// Lock is a held projection lock; see [Locker].
type Lock interface {
	// Lost returns a channel that is closed when the lock is lost without
	// Release being called, for example because the database connection
	// holding it dropped. The runner stops processing at the next batch
	// boundary when that happens, because another instance may already
	// have taken over.
	Lost() <-chan struct{}

	// Owner identifies the holder, for example a host name and process
	// ID. It is published in [Status.Owner].
	Owner() string

	// Release gives the lock up, so another instance can take over
	// immediately instead of after a timeout.
	Release(ctx context.Context) error
}

// DeadLetterStore is optionally implemented by a [CheckpointStore] to
// record events the runner skipped under [WithErrorPolicy], so they can be
// inspected and replayed after the cause is fixed.
type DeadLetterStore interface {
	// SaveDeadLetter records that the projection called name skipped env
	// because its handler returned cause. It is called inside the unit of
	// work of the batch that skips the event, so the record and the
	// checkpoint moving past the event are committed together.
	SaveDeadLetter(ctx context.Context, name string, env *eventsourcing.Envelope, cause error) error
}

// ErrorPolicy says what a [Runner] does when a handler returns an error for
// an event; set it with [WithErrorPolicy].
//
// Whatever the policy, an error from the store or the checkpoint store
// always stalls the runner, since it is not about any particular event.
type ErrorPolicy int

const (
	// StallOnError stops the projection at the failing event: the runner
	// reports [Stalled], backs off and retries until the handler succeeds
	// or the projection is rebuilt. Nothing is skipped silently, at the
	// cost of the projection not progressing until the cause is fixed.
	// This is the default.
	StallOnError ErrorPolicy = iota

	// SkipWhenLive skips failing events while the projection is [Live],
	// and stalls on them while it is catching up. It keeps a running
	// projection available while a rebuild, which can be verified before
	// going live, stays strict.
	SkipWhenLive

	// SkipAlways skips failing events in every phase.
	SkipAlways
)

// String returns the policy's name.
func (p ErrorPolicy) String() string {
	switch p {
	case StallOnError:
		return "stall_on_error"
	case SkipWhenLive:
		return "skip_when_live"
	case SkipAlways:
		return "skip_always"
	default:
		return "unknown"
	}
}
