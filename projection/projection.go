package projection

import (
	"context"
	"hash/fnv"
	"log/slog"
	"time"

	"github.com/terraskye/eventsourcing"
)

// Default values for [Definition]'s tuning fields.
const (
	DefaultBatchSize    = 500
	DefaultPollInterval = time.Second
	DefaultClaimTTL     = 15 * time.Second
)

// SegmentKey picks which segment an event belongs to in a segmented
// projection; see [Definition.Segments]. It must return the same key for
// every event that touches the same read-model rows, so that no two
// segments ever write the same row.
type SegmentKey func(env *eventsourcing.Envelope) string

// ByStreamID is a [SegmentKey] that partitions on the envelope's StreamID,
// for read models whose rows are keyed by stream.
func ByStreamID(env *eventsourcing.Envelope) string { return env.StreamID }

// SegmentOf maps key to a segment in [0, segments). It is stable across
// processes and releases, so every instance routes an event to the same
// segment.
func SegmentOf(key string, segments int) int {
	if segments <= 1 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(key)) // hash.Hash.Write never returns an error.
	return int(h.Sum32() % uint32(segments))
}

// ErrorPolicy decides what the runner does when a handler returns an error
// for an event.
//
// A skipped event parks its stream: the runner skips that stream's later
// events too, recording each as a dead letter, so a read model never sees
// OrderShipped without the OrderPlaced it skipped. Other streams carry on.
// [Runner.ReplayStream] applies a parked stream's events once the cause is
// fixed.
type ErrorPolicy int

const (
	// StallOnError stops the segment at the failing event: the runner
	// reports [Stalled], backs off and retries until the handler succeeds
	// or the projection is rebuilt. The backoff grows from 0.5s to at most
	// 30s, jittered, and never gives up. Nothing is skipped silently.
	// Default.
	StallOnError ErrorPolicy = iota

	// SkipWhenLive skips failing events while [Live], and stalls on them
	// while catching up. Keeps a running projection available while a
	// rebuild, which can be verified before going live, stays strict.
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

// Definition describes a projection: what it handles and how it runs. The
// zero value for every optional field picks sensible defaults.
//
// A projection is usually defined next to the read model it writes, by a
// constructor in the same package, and handed to [NewRunner] by main:
//
//	func NewProjection(list *TaskList) projection.Definition {
//		return projection.Definition{
//			Name:       "task-list",
//			Handler:    eventsourcing.NewEventHandlerFunc(list.handle),
//			EventTypes: []string{"TaskCreated", "TaskCompleted"},
//		}
//	}
//
//	r := projection.NewRunner(tasklist.NewProjection(list), reader, store)
//
// A durable projection adds Version and Reset so it can be rebuilt:
//
//	projection.Definition{
//		Name:       "task-list",
//		Handler:    eventsourcing.NewEventHandlerFunc(handle),
//		EventTypes: []string{"TaskCreated", "TaskCompleted"},
//		Version:    2,
//		Reset:      reset,
//	}
//
// A projection whose one instance cannot keep up is segmented:
//
//	projection.Definition{
//		Name:       "order-summaries-v1",
//		Handler:    eventsourcing.NewEventHandlerFunc(handle),
//		EventTypes: []string{"OrderPlaced", "OrderShipped"},
//		Segments:   64,
//		SegmentKey: projection.ByStreamID,
//	}
//
// Nothing else changes. A segmented projection is just the same loop
// scoped to each segment the store lets this instance claim.
//
// Definition is a plain value, so main can still adjust deployment
// settings, such as PollInterval or a logging wrapper around Handler,
// before passing it to NewRunner.
type Definition struct {
	// Name identifies the projection. It keys the checkpoint and the
	// claim, so it must be unique per read model and stable across
	// deploys; renaming a projection makes it start from scratch.
	Name string

	// Handler handles every event the runner reads, in log order. Wrap a
	// function that switches on the event type with
	// [eventsourcing.NewEventHandlerFunc], or pass an
	// [*eventsourcing.EventGroupProcessor].
	Handler eventsourcing.EventHandler

	// EventTypes lists the registered names of the event types Handler
	// handles; the runner reads only these. For an
	// [*eventsourcing.EventGroupProcessor], pass its StreamFilter.
	//
	// Adding an event type is a change to how the read model is written:
	// bump Version with it. Otherwise the runner carries on from its
	// checkpoint, and every event of the new type stored before it is
	// never applied.
	EventTypes []string

	// Version is the version of the projection's code, stored with the
	// checkpoint. When the runner starts a segment and finds a stored
	// version different from this one, the segment is rebuilt before
	// processing new events. Bump the version whenever the projection
	// starts writing the read model differently; see Reset. A change to
	// EventTypes is detected without a bump; see [ErrEventTypesChanged].
	//
	// A rebuild empties the read model until the runner has caught up
	// again. For a large log, build the new version under a new Name
	// instead and switch queries over once it is live.
	//
	// A rebuild covers every segment of a segmented projection at once:
	// Reset wipes the whole read model, so every segment starts over, and
	// instances still working a segment are fenced off and resume from
	// the reset checkpoint.
	//
	// Only raise Version. A runner whose Version is below the stored one,
	// such as an instance of the previous deployment during a rollout,
	// stands by instead of rebuilding back.
	//
	// Default 0: no automatic rebuilds (Rebuild can still be called).
	// Setting a non-zero Version requires Reset.
	Version int

	// Reset wipes the read model so the projection can be rebuilt. It runs
	// in the unit of work that moves the checkpoint back to zero, through
	// the same context handlers use (e.g. postgres.Tx(ctx)):
	//
	//	func reset(ctx context.Context) error {
	//		_, err := postgres.Tx(ctx).Exec(ctx, "TRUNCATE tasks")
	//		return err
	//	}
	//
	// Required for Version > 0 and for [Runner.Rebuild].
	Reset func(ctx context.Context) error

	// Segments is how many segments the projection is split into. 0 or 1
	// means a single-segment projection that one instance runs at a time;
	// a value above 1 segments it, with instances sharing the segments
	// through the [Store]'s claim mechanism.
	//
	// Set this only when one instance cannot keep up. The segment count is
	// fixed per name; changing it takes a new name.
	//
	// Segments parallelize the handlers, not the reads: every segment reads
	// the filtered log on its own and drops the events of other segments,
	// so N segments read the log N times. Every segment also writes a
	// heartbeat to the store once per PollInterval while idle, so many
	// segments across many instances add a steady write load. A read model
	// that combines events of many streams into one row, such as a total
	// or a count, cannot be segmented: no SegmentKey keeps that row within
	// one segment.
	Segments int

	// SegmentKey picks which segment an event belongs to; required when
	// Segments > 1. [ByStreamID] fits read models whose rows are per
	// stream. Pick a key that keeps every read-model row within one
	// segment, or two segments will race on the same row.
	SegmentKey SegmentKey

	// OnError is what the runner does when a handler fails on an event;
	// see [ErrorPolicy]. Default [StallOnError].
	OnError ErrorPolicy

	// Logger receives the runner's lifecycle: a segment claimed, released
	// or lost (Info, Warn), a rebuild started and finished (Info), an event
	// skipped (Warn), and a segment stalled with its error (Error). Every
	// record carries projection, segment, owner, position and head. The
	// runner logs nothing per applied event and nothing while idle; to log
	// handler calls, wrap Handler, for example with
	// logging.WithEventLogging. Default slog.Default(); pass
	// slog.New(slog.DiscardHandler) for silence.
	Logger *slog.Logger

	// Owner identifies this instance to the store, so other instances see
	// who holds a segment. Claims are fenced by [Lease.Epoch], so two
	// runners under the same Owner, such as a process restarted under a
	// fixed pod name, still never hold a segment at once. Defaults to
	// "hostname/pid/random", unique per Runner.
	Owner string

	// BatchSize is the maximum number of events read and committed in one
	// unit of work. Default [DefaultBatchSize].
	BatchSize int

	// PollInterval is how often the runner reads again when nothing woke it
	// earlier. Also how often it tries to claim segments it does not own,
	// and how often an idle or stalled segment refreshes its claim, so it
	// must be shorter than ClaimTTL. Default [DefaultPollInterval].
	PollInterval time.Duration

	// ClaimTTL is how long a claim stays valid without a Commit or
	// SaveStatus to refresh it. A crashed instance's segments become
	// available again after this long. Default [DefaultClaimTTL].
	//
	// The heartbeat is refreshed when a batch commits, not while its
	// handlers run. Keep ClaimTTL above the time the slowest batch takes,
	// or lower BatchSize. The Postgres store locks the segment for the
	// whole batch, so a slow batch delays a takeover rather than losing
	// its work; the memory store does not, and another instance can take
	// the segment over halfway through the batch.
	ClaimTTL time.Duration
}
