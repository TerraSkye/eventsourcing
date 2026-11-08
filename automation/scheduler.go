package automation

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/terraskye/eventsourcing/projection"
)

// ClaimRequest tells a todo list what to lease.
//
// Segments is empty for [TodoList.Claim]: the claim then covers the whole
// todo list. For [Segmented.ClaimSegments] it names the segments this
// instance holds, and the claim returns items for those segments only, at
// most MaxPerKey each.
type ClaimRequest struct {
	Segments  []string
	MaxPerKey int
	Lease     time.Duration
}

// TodoList is what an automation works off: a repository over the todo
// table. Claim is all it needs. A list can offer more by implementing
// [Segmented]: one owner per segment, spread over the instances. The
// runner detects that at startup and logs what it found.
//
// Claim reads pending items and leases them for req.Lease, so that other
// instances skip them until the lease expires; req.Segments is empty. The
// query's ORDER BY decides processing order. A claim must be atomic with
// respect to other instances: two instances claiming at the same time must
// never both get the same item; in SQL, FOR UPDATE SKIP LOCKED does this.
// An item whose lease expired without its work succeeding is pending again
// and will be claimed again. Claim returns an empty slice, not an error,
// when nothing is pending. automationtest.ClaimAcceptanceTest checks an
// implementation against this contract.
type TodoList[T any] interface {
	Claim(ctx context.Context, req ClaimRequest) ([]T, error)
}

// Segmented is implemented by a todo list that is divided into segments:
// a column the projection writes, either a shard number ([Shards]) or a
// business key such as a tenant ([Query]). Each segment is leased to one
// instance at a time, so per-segment limits hold across instances, and a
// [Deferred] from Work pauses one segment rather than the whole list.
//
// ClaimSegments is Claim for the segments in req.Segments, which is never
// empty: up to req.MaxPerKey items per segment, grouped by segment. The
// list scans the segment column anyway, so it groups; the runner never
// has to look inside an item. Segments without pending items are simply
// absent from the result.
type Segmented[T any] interface {
	TodoList[T]
	Segments() Segmenter
	ClaimSegments(ctx context.Context, req ClaimRequest) ([]Claimed[T], error)
}

// Claimed is what [Segmented.ClaimSegments] returns for one segment, in
// the order the items should be processed.
type Claimed[T any] struct {
	Segment string
	Items   []T
}

// Segmenter says which segments exist right now. Use [Shards] for a fixed
// number of hash buckets or [Query] for segments discovered from the work.
type Segmenter interface {
	segments(ctx context.Context) ([]Segment, error)
}

// Segment is one segment with pending work. Group is optional: when set,
// the scheduler round-robins across groups before segments, so that a
// tenant with forty segments and a tenant with one get the same number of
// turns.
type Segment struct {
	Key   string
	Group string
}

type shards int

func (n shards) segments(context.Context) ([]Segment, error) {
	out := make([]Segment, int(n))
	for i := range out {
		out[i] = Segment{Key: strconv.Itoa(i)}
	}
	return out, nil
}

// Shards segments a list into n fixed buckets, "0" to "n-1". The
// projection writes each item's bucket with [ShardKey]; Claim filters on
// it. Every bucket is a candidate on every poll, so no discovery query is
// needed; the cost is that a Deferred pauses a whole bucket.
func Shards(n int) Segmenter {
	if n <= 0 {
		panic("automation: Shards needs n > 0")
	}
	return shards(n)
}

// ShardKey returns id's bucket among n, as a string: the same FNV-32a
// bucket [projection.SegmentOf] gives, so a todo-list row and the
// projection that wrote it agree.
func ShardKey(id string, n int) string {
	return strconv.Itoa(projection.SegmentOf(id, n))
}

type query func(context.Context) ([]Segment, error)

func (q query) segments(ctx context.Context) ([]Segment, error) { return q(ctx) }

// Query segments a list by what currently has pending work, typically
//
//	SELECT DISTINCT tenant_id FROM todo WHERE <pending>
//
// with the tenant in Group when tenant fairness is wanted. fn is called on
// every poll; it should return only segments with work, and only those.
func Query(fn func(ctx context.Context) ([]Segment, error)) Segmenter { return query(fn) }

// WorkFunc does the work for one claimed item: a call to an external
// system, one or more commands, a fan-out to either. It ends in events
// that, once the todo-list projection applies them, remove the item.
//
// Returning nil marks the item done. Returning an error leaves the item
// where it is; it is claimed again once its lease expires. Returning
// [Deferred] pauses the item's whole segment until the given time without
// counting a failure: that is what a 429 with Retry-After means. Work can
// be repeated, so it must be idempotent; see the package documentation.
type WorkFunc[T any] func(ctx context.Context, item T) error

// DeferredError is returned by [WorkFunc] to report that the item's
// segment is throttled until Until. It is not counted as a failure.
//
// On an unsegmented list there is one segment, so a DeferredError pauses
// the whole automation on every instance. For a 429 from one integration
// on such a list, return a plain error or segment the list by integration.
type DeferredError struct {
	Until time.Time
}

func (e *DeferredError) Error() string {
	if e.Until.IsZero() {
		return "deferred"
	}
	return fmt.Sprintf("deferred until %s", e.Until.Format(time.RFC3339))
}

// Deferred returns a *DeferredError. A zero until means "back off for the
// automation's Lease".
func Deferred(until time.Time) error { return &DeferredError{Until: until} }

// Item is one claimed item as a [Scheduler] hands it to the runner.
type Item[T any] struct {
	// Key is the segment, "" for an unsegmented list.
	Key string

	// Value is the item itself.
	Value T

	// Deadline is when the item's lease ends. The runner gives Work a
	// context with this deadline, so that work cannot outlive its lease.
	Deadline time.Time
}

// Batch is the items of one segment, in the order they should be
// dispatched. Items that share an ordering key run in that order; see
// [Definition.OrderingKey].
type Batch[T any] struct {
	Key   string
	Items []Item[T]
}

// OutcomeKind is what happened to an item.
type OutcomeKind int

const (
	// Done means Work returned nil: the item's events are recorded and the
	// projection will remove it.
	Done OutcomeKind = iota

	// Failed means Work returned an error. The item stays where it is and
	// is claimed again after its lease.
	Failed

	// DeferredOutcome means Work returned [Deferred]: the item's segment is
	// paused until Outcome.Until.
	DeferredOutcome
)

// Outcome reports an item's result to the [Scheduler].
type Outcome struct {
	Kind  OutcomeKind
	Until time.Time // for DeferredOutcome
	Err   error     // for Failed
}

// Scheduler is the pluggable half of an automation: it decides which items
// this instance works now, for which segments, and what happens when an
// item finishes. The runner keeps the other half: ordering-key dispatch,
// Work deadlines, outcomes, [Runner.Run] and [Runner.RunOnce].
//
// The default scheduler claims from the [TodoList] and, for a [Segmented]
// list, leases segments through a [Store]; see [Definition]. Another
// implementation can put the scheduling anywhere: a broker that hands out
// keyed work, a queue, a service. Such an implementation enforces the
// segment contract itself: at most one owner per segment at a time, and
// [Definition.Concurrency] items per segment.
//
// Each batch runs on its own: the runner calls Next again while earlier
// batches are still running, so that a slow segment does not hold up the
// others. Next, Done and Drained are therefore called from several
// goroutines at once, and an implementation must be safe for that.
//
// This interface may still change in minor releases before v1.0: it is
// published so that alternative schedulers can be written, and declared
// stable once a second implementation has fit it.
type Scheduler[T any] interface {
	// Next returns the items this instance may work now, grouped by
	// segment, or nothing when there is no eligible work. It must not
	// block waiting for work to appear; the runner waits between calls.
	// It must not return a batch for a segment whose previous batch has
	// not drained yet.
	Next(ctx context.Context) ([]Batch[T], error)

	// Done reports an item's outcome. For [DeferredOutcome] the scheduler
	// pauses the item's segment until Outcome.Until for every instance.
	Done(ctx context.Context, item Item[T], out Outcome) error

	// Drained reports that the batch Next returned for segment key ("" for
	// an unsegmented list) has finished: every item that was started has
	// had its Done, and the rest of the batch will not be started. The
	// segment may have work again from now on.
	Drained(ctx context.Context, key string) error

	// Close releases whatever the scheduler holds: segments, pins,
	// connections. The runner calls it once Run or RunOnce returns.
	Close(ctx context.Context) error
}
