// Package postgres stores projection checkpoints in PostgreSQL, in the same
// transaction as the read model they belong to.
//
// [Checkpoints] implements every store interface the projection package
// knows about: [projection.CheckpointStore] for exactly-once processing,
// [projection.StatusStore] so other processes can follow a projection,
// [projection.Locker] so each projection runs on one instance, and
// [projection.DeadLetterStore] for events skipped under an error policy,
// and [projection.PartitionedCheckpointStore] for partitioned projections.
// [Leases] is a [projection.LeaseStore] on the same database.
//
// Handlers write through the transaction the runner opened for their batch,
// which [Tx] returns:
//
//	func (p *Projector) OnTaskCreated(ctx context.Context, e *events.TaskCreated) error {
//		_, err := postgres.Tx(ctx).Exec(ctx,
//			`INSERT INTO tasks (id, title) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`,
//			e.TaskID, e.Title)
//		return err
//	}
//
//	taskList := projection.NewRunner("task-list", projector.EventHandlers(), store,
//		projection.WithCheckpoints(postgres.NewCheckpoints(pool)),
//	)
//
// The read model's tables and the tables in [Schema] must live in the same
// database, since they are written in one transaction.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/projection"
)

// errNotImplemented marks the parts of this package that are still a
// design draft: signatures and documentation only.
var errNotImplemented = errors.New("projection/postgres: not implemented yet")

// Schema creates the tables [Checkpoints] uses. It is idempotent; run it
// with your migrations, or once at startup.
//
// projection_checkpoints holds one row per projection, or per partition of
// a partitioned projection: its checkpoint, the fence of the partition's
// current owner, the projector version and replay boundary, and the status
// published for other processes. An unpartitioned projection uses partition
// 0 and fence 0. All columns exist from the start, including those used
// only by later features, so that upgrading never has to alter a table
// that every running projection writes to.
//
// projection_dead_letters holds events skipped under
// [projection.WithErrorPolicy].
//
// projection_leases and projection_members are used by [Leases] only; they
// may stay empty when leases are kept elsewhere.
const Schema = `
CREATE TABLE IF NOT EXISTS projection_checkpoints (
    name         TEXT        NOT NULL,
    partition    INT         NOT NULL DEFAULT 0,
    fence        BIGINT      NOT NULL DEFAULT 0,
    position     BIGINT      NOT NULL DEFAULT 0,
    version      INT         NOT NULL DEFAULT 0,
    replay_until BIGINT      NOT NULL DEFAULT 0,
    phase        TEXT        NOT NULL DEFAULT 'starting',
    head         BIGINT      NOT NULL DEFAULT 0,
    skipped      BIGINT      NOT NULL DEFAULT 0,
    last_error   TEXT,
    owner        TEXT,
    heartbeat    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (name, partition)
);

CREATE TABLE IF NOT EXISTS projection_dead_letters (
    id             BIGSERIAL   PRIMARY KEY,
    projection     TEXT        NOT NULL,
    global_version BIGINT      NOT NULL,
    event_id       UUID        NOT NULL,
    stream_id      TEXT        NOT NULL,
    event_type     TEXT        NOT NULL,
    error          TEXT        NOT NULL,
    skipped_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_projection_dead_letters_projection
    ON projection_dead_letters (projection, global_version);

CREATE TABLE IF NOT EXISTS projection_leases (
    projection    TEXT        NOT NULL,
    partition     INT         NOT NULL,
    owner         TEXT,
    lease_expires TIMESTAMPTZ,
    fence         BIGINT      NOT NULL DEFAULT 0,
    PRIMARY KEY (projection, partition)
);

CREATE TABLE IF NOT EXISTS projection_members (
    projection TEXT        NOT NULL,
    member     TEXT        NOT NULL,
    last_seen  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (projection, member)
);
`

type txKey struct{}

// Tx returns the transaction a [projection.Runner] opened for the batch
// being processed under ctx. Handlers, reset functions and buffers write
// the read model through it, so their writes commit together with the
// checkpoint.
//
// Tx panics if ctx carries no transaction, which means it was called
// outside a projection that uses [Checkpoints]: a wiring mistake, not a
// condition to handle at run time.
func Tx(ctx context.Context) pgx.Tx {
	tx, ok := ctx.Value(txKey{}).(pgx.Tx)
	if !ok {
		panic("projection/postgres: Tx called outside a projection batch")
	}
	return tx
}

// Checkpoints stores projection checkpoints, statuses, locks and dead
// letters in PostgreSQL. Create one with [NewCheckpoints] and pass it to
// [projection.WithCheckpoints]; one value can serve every projection in the
// database.
type Checkpoints struct {
	pool *pgxpool.Pool
}

// NewCheckpoints returns a [Checkpoints] that uses pool, which must connect
// to the database holding the read models. The tables in [Schema] must
// exist.
func NewCheckpoints(pool *pgxpool.Pool) *Checkpoints {
	return &Checkpoints{pool: pool}
}

// Load returns the checkpoint stored for name, or the zero
// [projection.Checkpoint] if there is none.
func (c *Checkpoints) Load(ctx context.Context, name string) (projection.Checkpoint, error) {
	var cp projection.Checkpoint
	err := c.pool.QueryRow(ctx, `
		SELECT position, version, replay_until FROM projection_checkpoints
		WHERE name = $1 AND partition = 0`, name).Scan(&cp.Position, &cp.Version, &cp.ReplayUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return projection.Checkpoint{}, nil
	}
	return cp, err
}

// Commit begins a transaction, runs fn with a ctx that carries it (see
// [Tx]), stores cp for name in the same transaction, and commits. If fn
// fails, the transaction is rolled back and the stored checkpoint is
// unchanged.
func (c *Checkpoints) Commit(ctx context.Context, name string, cp projection.Checkpoint, fn func(ctx context.Context) error) error {
	return pgx.BeginFunc(ctx, c.pool, func(tx pgx.Tx) error {
		if err := fn(context.WithValue(ctx, txKey{}, tx)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO projection_checkpoints (name, partition, position, version, replay_until)
			VALUES ($1, 0, $2, $3, $4)
			ON CONFLICT (name, partition) DO UPDATE
			SET position = EXCLUDED.position, version = EXCLUDED.version, replay_until = EXCLUDED.replay_until`,
			name, toInt64(cp.Position), cp.Version, toInt64(cp.ReplayUntil))
		return err
	})
}

// SaveStatus stores s for name, setting the heartbeat to the database's
// current time.
func (c *Checkpoints) SaveStatus(ctx context.Context, name string, s projection.Status) error {
	_, err := c.pool.Exec(ctx, `
		INSERT INTO projection_checkpoints AS c (name, partition, phase, head, skipped, last_error, owner, heartbeat)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), NULLIF($7, ''), now())
		ON CONFLICT (name, partition) DO UPDATE
		SET phase = EXCLUDED.phase, head = EXCLUDED.head, skipped = EXCLUDED.skipped,
		    last_error = EXCLUDED.last_error, owner = EXCLUDED.owner, heartbeat = now()`,
		name, s.Partition, s.Phase.String(), toInt64(s.Head), toInt64(s.Skipped), s.LastError, s.Owner)
	return err
}

// LoadStatus returns the status last saved for name, with Age computed by
// the database (now() minus the heartbeat), so the clocks of the machines
// involved do not matter.
func (c *Checkpoints) LoadStatus(ctx context.Context, name string) (projection.Status, bool, error) {
	rows, err := c.pool.Query(ctx, `
		SELECT position, version, replay_until, phase, head, skipped,
		       coalesce(last_error, ''), coalesce(owner, ''),
		       greatest(extract(epoch FROM now() - heartbeat), 0)::float8
		FROM projection_checkpoints WHERE name = $1 ORDER BY partition`, name)
	if err != nil {
		return projection.Status{}, false, err
	}
	defer rows.Close()

	var (
		agg   projection.Status
		n     int
		owner string
	)
	for rows.Next() {
		var (
			pos, replayUntil, head, skipped int64
			version                         int
			phase, lastError, rowOwner      string
			age                             float64
		)
		if err := rows.Scan(&pos, &version, &replayUntil, &phase, &head, &skipped, &lastError, &rowOwner, &age); err != nil {
			return projection.Status{}, false, err
		}
		s := projection.Status{
			Phase:     parsePhase(phase),
			Position:  uint64(pos),
			Head:      uint64(head),
			Version:   version,
			Replaying: pos < replayUntil,
			Skipped:   uint64(skipped),
			LastError: lastError,
			Age:       time.Duration(age * float64(time.Second)),
		}
		if n == 0 {
			agg = s
			owner = rowOwner
		} else {
			agg.Position = min(agg.Position, s.Position)
			agg.Head = max(agg.Head, s.Head)
			agg.Version = min(agg.Version, s.Version)
			agg.Replaying = agg.Replaying || s.Replaying
			agg.Skipped += s.Skipped
			agg.Age = max(agg.Age, s.Age)
			if phaseRank(s.Phase) > phaseRank(agg.Phase) {
				agg.Phase, agg.LastError = s.Phase, s.LastError
			}
			if rowOwner != owner {
				owner = ""
			}
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return projection.Status{}, false, err
	}
	if n == 0 {
		return projection.Status{}, false, nil
	}
	agg.Owner = owner
	if n > 1 {
		agg.Partitions = n
	}
	return agg, true, nil
}

func parsePhase(s string) projection.Phase {
	for p := projection.Starting; p <= projection.Stalled; p++ {
		if p.String() == s {
			return p
		}
	}
	return projection.Starting
}

// phaseRank orders phases from furthest along to furthest behind, the same
// way the projection package combines partitions.
func phaseRank(p projection.Phase) int {
	switch p {
	case projection.Live:
		return 0
	case projection.CatchingUp:
		return 1
	case projection.Standby:
		return 2
	case projection.Starting:
		return 3
	default:
		return 4
	}
}

// toInt64 converts a position to a BIGINT. Positions never come near
// MaxInt64, so one that does is a corrupted value, not a condition to
// handle.
func toInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		panic(fmt.Sprintf("projection/postgres: position %d does not fit in a BIGINT", v))
	}
	return int64(v)
}

// TryLock tries to take a session-level advisory lock for name on a
// dedicated connection from the pool, which it holds until the lock is
// released or lost.
//
// The returned lock watches that connection. If the connection drops (a
// database restart or failover), the lock is gone on the server side, and
// Lost is closed so the runner stops processing before another instance
// takes over. Without that watch, a runner could keep believing it holds a
// lock that another instance has already acquired.
func (c *Checkpoints) TryLock(ctx context.Context, name string) (projection.Lock, bool, error) {
	panic(errNotImplemented)
}

// SaveDeadLetter records in projection_dead_letters that the projection
// called name skipped env because of cause. It writes through the batch's
// transaction, taken from ctx.
func (c *Checkpoints) SaveDeadLetter(ctx context.Context, name string, env *eventsourcing.Envelope, cause error) error {
	panic(errNotImplemented)
}

// InitPartitions inserts a row at position 0 for every partition of name
// that has none.
//
// With the rows in place, the number of partitions behind the head of the
// log is one query, which suits an autoscaler such as KEDA (with a target
// of about the partitions per instance, and activation at 0, so the program
// scales to zero when nothing is behind):
//
//	SELECT count(*) FROM projection_checkpoints
//	WHERE name = 'order-summaries-v1' AND position < $head
func (c *Checkpoints) InitPartitions(ctx context.Context, name string, partitions int) error {
	_, err := c.pool.Exec(ctx, `
		INSERT INTO projection_checkpoints (name, partition)
		SELECT $1, g FROM generate_series(0, $2::int - 1) g
		ON CONFLICT DO NOTHING`, name, partitions)
	return err
}

// Acquire raises the partition's fence to l.Fence and returns its
// checkpoint, in one statement:
//
//	INSERT INTO projection_checkpoints AS c (name, partition, fence) VALUES ($1, $2, $3)
//	ON CONFLICT (name, partition) DO UPDATE SET fence = EXCLUDED.fence
//	WHERE c.fence <= EXCLUDED.fence
//	RETURNING c.position, c.version, c.replay_until
//
// No row returned means a higher fence is recorded: [projection.ErrLeaseLost].
// The update waits for the row lock held by an earlier owner's
// [Checkpoints.CommitPartition], so Acquire returns what that commit stored.
func (c *Checkpoints) Acquire(ctx context.Context, l projection.Lease) (projection.Checkpoint, error) {
	var (
		cp                    projection.Checkpoint
		position, replayUntil int64
	)
	err := c.pool.QueryRow(ctx, `
		INSERT INTO projection_checkpoints AS c (name, partition, fence) VALUES ($1, $2, $3)
		ON CONFLICT (name, partition) DO UPDATE SET fence = EXCLUDED.fence
		WHERE c.fence <= EXCLUDED.fence
		RETURNING c.position, c.version, c.replay_until`,
		l.Projection, l.Partition, l.Fence).Scan(&position, &cp.Version, &replayUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return projection.Checkpoint{}, projection.ErrLeaseLost
	}
	if err != nil {
		return projection.Checkpoint{}, err
	}
	cp.Position, cp.ReplayUntil = uint64(position), uint64(replayUntil)
	return cp, nil
}

// CommitPartition begins a transaction and first stores cp for l's
// partition, conditional on the fence:
//
//	UPDATE projection_checkpoints SET position = $1 ...
//	WHERE name = $2 AND partition = $3 AND fence = $4
//
// No row updated means another instance took over: the transaction is
// rolled back and [projection.ErrLeaseLost] returned. Otherwise the row
// stays locked while fn runs with a ctx that carries the transaction (see
// [Tx]), and the transaction commits if fn succeeds.
//
// Storing the checkpoint before fn, rather than after, is what makes it
// safe: the fence is checked before anything is written, and the row lock
// makes a new owner's [Checkpoints.Acquire] wait for this batch.
func (c *Checkpoints) CommitPartition(ctx context.Context, l projection.Lease, cp projection.Checkpoint, fn func(ctx context.Context) error) error {
	return pgx.BeginFunc(ctx, c.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE projection_checkpoints
			SET position = $1, version = $2, replay_until = $3
			WHERE name = $4 AND partition = $5 AND fence = $6`,
			toInt64(cp.Position), cp.Version, toInt64(cp.ReplayUntil), l.Projection, l.Partition, l.Fence)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return projection.ErrLeaseLost
		}
		return fn(context.WithValue(ctx, txKey{}, tx))
	})
}

var (
	_ projection.CheckpointStore            = (*Checkpoints)(nil)
	_ projection.PartitionedCheckpointStore = (*Checkpoints)(nil)
	_ projection.StatusStore                = (*Checkpoints)(nil)
	_ projection.Locker                     = (*Checkpoints)(nil)
	_ projection.DeadLetterStore            = (*Checkpoints)(nil)
)
