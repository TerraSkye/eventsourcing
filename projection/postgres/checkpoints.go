// Package postgres stores projection checkpoints in PostgreSQL, in the same
// transaction as the read model they belong to.
//
// [Checkpoints] implements every store interface the projection package
// knows about: [projection.CheckpointStore] for exactly-once processing,
// [projection.StatusStore] so other processes can follow a projection,
// [projection.Locker] so each projection runs on one instance, and
// [projection.DeadLetterStore] for events skipped under an error policy.
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
// projection_checkpoints holds one row per projection: its checkpoint, the
// projector version and replay boundary, and the status published for
// other processes. All columns exist from the start, including those used
// only by later features, so that upgrading never has to alter a table
// that every running projection writes to.
//
// projection_dead_letters holds events skipped under
// [projection.WithErrorPolicy].
const Schema = `
CREATE TABLE IF NOT EXISTS projection_checkpoints (
    name         TEXT        PRIMARY KEY,
    position     BIGINT      NOT NULL DEFAULT 0,
    version      INT         NOT NULL DEFAULT 0,
    replay_until BIGINT      NOT NULL DEFAULT 0,
    phase        TEXT        NOT NULL DEFAULT 'starting',
    head         BIGINT      NOT NULL DEFAULT 0,
    skipped      BIGINT      NOT NULL DEFAULT 0,
    last_error   TEXT,
    owner        TEXT,
    heartbeat    TIMESTAMPTZ NOT NULL DEFAULT now()
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
	panic(errNotImplemented)
}

// Commit begins a transaction, runs fn with a ctx that carries it (see
// [Tx]), stores cp for name in the same transaction, and commits. If fn
// fails, the transaction is rolled back and the stored checkpoint is
// unchanged.
func (c *Checkpoints) Commit(ctx context.Context, name string, cp projection.Checkpoint, fn func(ctx context.Context) error) error {
	panic(errNotImplemented)
}

// SaveStatus stores s for name, setting the heartbeat to the database's
// current time.
func (c *Checkpoints) SaveStatus(ctx context.Context, name string, s projection.Status) error {
	panic(errNotImplemented)
}

// LoadStatus returns the status last saved for name, with Age computed by
// the database (now() minus the heartbeat), so the clocks of the machines
// involved do not matter.
func (c *Checkpoints) LoadStatus(ctx context.Context, name string) (projection.Status, bool, error) {
	panic(errNotImplemented)
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

var (
	_ projection.CheckpointStore = (*Checkpoints)(nil)
	_ projection.StatusStore     = (*Checkpoints)(nil)
	_ projection.Locker          = (*Checkpoints)(nil)
	_ projection.DeadLetterStore = (*Checkpoints)(nil)
)
