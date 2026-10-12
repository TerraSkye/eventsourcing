package projection

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/terraskye/eventsourcing"
)

var (
	// ErrLeaseLost is returned by a [PartitionedCheckpointStore] when another
	// instance has taken over the partition: its lease carries a higher
	// fence. The runner stops working on the partition and lets the other
	// instance continue from the last committed checkpoint.
	ErrLeaseLost = errors.New("projection: partition lease lost")

	// ErrPartitionsChanged is returned by [LeaseStore.Init] when the
	// projection already exists with a different number of partitions.
	// Events would map to different partitions than the stored checkpoints
	// were written for, so changing the count takes a new projection name;
	// see [WithPartitions].
	ErrPartitionsChanged = errors.New("projection: partition count differs from the stored one")

	// ErrPartitioned is returned by [Runner.Rebuild] on a runner created
	// with [WithPartitions]. A partitioned projection is rebuilt under a new
	// name instead; see [WithPartitions].
	ErrPartitioned = errors.New("projection: partitioned projections are rebuilt under a new name")
)

// PartitionKey picks what a partitioned projection partitions on; see
// [WithPartitions].
//
// It must return the same key for every event that touches the same
// read-model rows, so that no two partitions ever write the same row: the
// stream ID for rows per stream, an aggregate ID shared across streams, a
// tenant ID for per-tenant totals.
type PartitionKey func(env *eventsourcing.Envelope) string

// ByStreamID partitions on [eventsourcing.Envelope.StreamID], for read models
// with rows per stream.
func ByStreamID(env *eventsourcing.Envelope) string { return env.StreamID }

// PartitionOf maps key to a partition in [0, partitions). It is stable
// across processes and releases, so every instance routes an event to the
// same partition.
//
// Lag queries and handlers that need to know which partition a row belongs
// to can call it with the same key the projection uses.
func PartitionOf(key string, partitions int) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return int(h.Sum32() % uint32(partitions))
}

// WithPartitions splits the projection into n partitions that run spread
// over every instance of the program, instead of on one instance at a time.
// key decides which partition an event belongs to; leases decides which
// instance runs which partition.
//
// Use it when one instance cannot keep up with a projection, or to make a
// projection's throughput grow with the number of instances. A projection
// that keeps up on one instance is simpler without it.
//
// # How it runs
//
// Every instance heartbeats to leases and claims its fair share of the
// partitions: n divided by the number of live instances, rounded up. When an
// instance joins, others release partitions until the shares are even; when
// one shuts down, it hands its partitions over immediately; when one
// crashes, its leases expire after the lease TTL ([WithLeaseTTL]) and are
// claimed by the others. Scaling the program up and down, including from
// and to zero, just moves partitions around.
//
// Each instance reads the global log once, however many partitions it owns,
// and routes every event to the partition its key maps to. Within a
// partition, events are applied in log order, one batch per unit of work,
// exactly as an unpartitioned projection applies them. Events of different
// partitions are applied concurrently and in no particular order relative to
// each other, which is why the key must keep every read-model row in a
// single partition.
//
// # Fencing
//
// A lease alone cannot stop an instance that was paused (a long GC, a
// frozen VM) from waking up and committing after its lease expired and
// another instance took over. Every claim therefore carries a fence that is
// higher than any fence issued for that partition before, and the
// checkpoint store, which must implement [PartitionedCheckpointStore],
// records the fence of the current owner next to the partition's
// checkpoint. A commit with a lower fence fails with [ErrLeaseLost] in the
// same unit of work as the read-model writes, so they are rolled back. When
// a new owner takes over while the old one is in the middle of a batch, it
// waits for that batch and resumes after it: nothing is lost and nothing is
// applied twice.
//
// Leases may live elsewhere than the read model, in Redis for example, to
// keep heartbeats off the database; checkpoints and fences always live with
// the read model, since they must commit with it.
//
// # Idle partitions
//
// A partition advances its checkpoint even when none of the events read
// belong to it, so every partition's lag reaches zero, [Runner.WaitUntil]
// returns for positions in other partitions, and an autoscaler that counts
// lagging partitions can scale the program to zero.
//
// # Status and liveness
//
// The projection is live when every partition is live, wherever it runs.
// An instance only knows about the partitions it owns, so on a partitioned
// projection [Runner.Live] and [Runner.WaitUntil] read the published
// statuses of all partitions, which requires a checkpoint store that
// implements [StatusStore]. [Status.Position] is the lowest checkpoint of
// all partitions: every event up to it has been applied, whichever
// partition it belongs to.
//
// # Rebuilding and changing the partition count
//
// A partitioned projection is rebuilt by starting it under a new name, with
// new tables, next to the old one. Once the new one is live, switch reads
// over and remove the old one. [WithVersion], [WithReset] and
// [Runner.Rebuild] wipe the whole read model from one instance, which a
// partitioned projection cannot do while other instances write to it, so
// NewRunner panics on [WithVersion] combined with WithPartitions, and
// Rebuild returns [ErrPartitioned].
//
// The same goes for n: changing it maps events to different partitions, so
// it takes a new name too. A runner that finds the projection stored with a
// different count stalls with [ErrPartitionsChanged].
//
// # Choosing n
//
// n caps how many instances can share the work: with more instances than
// partitions, the extra instances only wait in [Standby]. Pick n well above
// the largest number of instances you expect, 32 or 64 for instance; idle
// partitions cost one checkpoint write per idle interval each.
//
// NewRunner panics if n is less than 1, key or leases is nil, the
// checkpoint store does not implement [PartitionedCheckpointStore], or the
// runner also has [WithVersion], [WithErrorPolicy] with a skipping policy,
// [WithOnCatchingUp] or [WithOnLive]; the last three are not supported on
// partitioned projections yet.
func WithPartitions(n int, key PartitionKey, leases LeaseStore) Option {
	return func(c *config) {
		c.partitions = n
		c.partitionKey = key
		c.leases = leases
	}
}

// WithMember sets how this instance identifies itself to the [LeaseStore]
// of a partitioned projection; see [WithPartitions]. It must be unique among
// the running instances and is published in [Status.Owner]. In Kubernetes,
// the pod name is a good choice.
//
// The default is the host name and process ID.
func WithMember(id string) Option {
	return func(c *config) { c.member = id }
}

// WithLeaseTTL sets how long a partition lease lasts without being renewed;
// see [WithPartitions]. The runner renews its leases every third of it. The
// default is 15 seconds.
//
// A crashed instance's partitions stand still for up to this long before
// another instance takes them over. A shorter TTL shortens that gap at the
// cost of more frequent heartbeats. Every batch must commit well within the
// TTL, so very large batches ([WithBatchSize]) need a longer one.
func WithLeaseTTL(d time.Duration) Option {
	return func(c *config) { c.leaseTTL = d }
}

// Lease is an instance's claim on one partition of a projection.
type Lease struct {
	// Projection is the projection's name.
	Projection string

	// Partition is the partition number, in [0, partitions).
	Partition int

	// Member identifies the instance holding the lease; see [WithMember].
	Member string

	// Fence increases on every claim of the partition, so a newer owner
	// always holds a higher fence than any earlier one. See
	// [WithPartitions].
	Fence int64
}

// LeaseStore records which instance runs which partition of a partitioned
// projection, and which instances are alive; see [WithPartitions].
//
// Lease timing must use the store's own clock, so clock differences between
// instances do not affect who owns what. The postgres package provides a
// LeaseStore on the read-model database.
type LeaseStore interface {
	// Init creates the leases for a projection with the given number of
	// partitions. It is idempotent, and returns [ErrPartitionsChanged] if
	// the projection exists with a different number.
	Init(ctx context.Context, projection string, partitions int) error

	// Heartbeat records member as alive for ttl and returns how many
	// members are alive, itself included.
	Heartbeat(ctx context.Context, projection, member string, ttl time.Duration) (live int, err error)

	// Claim takes up to n partitions that are free or whose lease expired,
	// leasing them to member for ttl. Every claim must return a fence
	// strictly greater than any fence issued before for that partition.
	// Two members claiming at the same time never get the same partition.
	Claim(ctx context.Context, projection, member string, n int, ttl time.Duration) ([]Lease, error)

	// Renew extends every lease member holds by ttl and returns the fence
	// of each partition it still holds. A partition the member thinks it
	// holds but that is missing, or has another fence, was taken over.
	Renew(ctx context.Context, projection, member string, ttl time.Duration) (fences map[int]int64, err error)

	// Release gives l back so another member can claim it right away. It
	// does nothing if l has changed hands since.
	Release(ctx context.Context, l Lease) error

	// Leave releases every lease member holds and removes it from the live
	// members.
	Leave(ctx context.Context, projection, member string) error
}

// PartitionedCheckpointStore is implemented by a [CheckpointStore] that can
// store one checkpoint per partition, fenced by the partition's lease. A
// runner created with [WithPartitions] requires it.
type PartitionedCheckpointStore interface {
	CheckpointStore

	// InitPartitions creates a checkpoint at the start of the log for every
	// partition of the projection that does not have one. It is idempotent.
	// Checkpoints must exist before any partition runs, so that lag queries
	// see a new projection as behind.
	InitPartitions(ctx context.Context, name string, partitions int) error

	// Acquire takes over l's partition and returns its committed
	// checkpoint. It records l.Fence as the partition's fence, which makes
	// every later commit with a lower fence fail. If an earlier owner is in
	// the middle of a commit, Acquire waits for it to finish and returns the
	// checkpoint it committed.
	//
	// Acquire returns [ErrLeaseLost] if a higher fence is already recorded.
	// Acquiring again with the same lease is allowed.
	Acquire(ctx context.Context, l Lease) (Checkpoint, error)

	// CommitPartition is [CheckpointStore.Commit] for one partition: it runs
	// fn inside a new unit of work, stores cp for l's partition in it, and
	// commits. It checks l.Fence before running fn, and keeps the
	// partition locked against [PartitionedCheckpointStore.Acquire] until
	// the unit of work ends.
	//
	// It returns [ErrLeaseLost], and keeps nothing fn did, if another
	// instance has acquired the partition with a higher fence.
	CommitPartition(ctx context.Context, l Lease, cp Checkpoint, fn func(ctx context.Context) error) error
}

// PartitionFromContext returns the partition the event or batch being
// handled under ctx belongs to, and ok == false when the projection is not
// partitioned or ctx was not passed by a [Runner].
func PartitionFromContext(ctx context.Context) (partition int, ok bool) {
	p, ok := ctx.Value(partitionCtxKey).(int)
	return p, ok
}

func validatePartitions(name string, cfg config) {
	if cfg.partitions == 0 && cfg.partitionKey == nil && cfg.leases == nil {
		return
	}
	switch {
	case cfg.partitions < 1:
		panic(fmt.Sprintf("projection %s: WithPartitions requires at least 1 partition, got %d", name, cfg.partitions))
	case cfg.partitionKey == nil:
		panic(fmt.Sprintf("projection %s: WithPartitions requires a partition key", name))
	case cfg.leases == nil:
		panic(fmt.Sprintf("projection %s: WithPartitions requires a lease store", name))
	case cfg.version != 0:
		panic(fmt.Sprintf("projection %s: WithVersion cannot be combined with WithPartitions; rebuild under a new name", name))
	case cfg.errorPolicy != StallOnError:
		panic(fmt.Sprintf("projection %s: WithErrorPolicy is not supported with WithPartitions yet", name))
	case cfg.onCatchingUp != nil || cfg.onLive != nil:
		panic(fmt.Sprintf("projection %s: WithOnCatchingUp and WithOnLive are not supported with WithPartitions yet", name))
	case cfg.leaseTTL <= 0:
		panic(fmt.Sprintf("projection %s: WithLeaseTTL must be positive", name))
	}
	if _, ok := cfg.checkpoints.(PartitionedCheckpointStore); !ok {
		panic(fmt.Sprintf("projection %s: WithPartitions requires a checkpoint store that implements PartitionedCheckpointStore", name))
	}
}
