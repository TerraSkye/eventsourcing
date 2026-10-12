// Package postgres is the Postgres [projection.Store].
//
// It persists everything a projection needs — checkpoints, segment claims,
// per-segment status, and (optionally) skipped-event records — in two
// tables, and runs each Commit inside a single Postgres transaction, so
// the read model's writes and the checkpoint move land together.
//
// Handlers reach the batch's transaction with [Tx]:
//
//	func (p *Projector) OnTaskCreated(ctx context.Context, e *events.TaskCreated) error {
//		_, err := postgres.Tx(ctx).Exec(ctx,
//			`INSERT INTO tasks (id, title) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`,
//			e.TaskID, e.Title)
//		return err
//	}
//
// The read model's tables and the tables [Schema] creates must live in the
// same database, since they are written in one transaction.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/terraskye/eventsourcing/projection"
)

// Schema creates the tables [Store] uses. It is idempotent; run it with
// your migrations, or once at startup.
//
// projection_segments holds one row per segment of each projection: its
// checkpoint, the owner currently claiming it with its heartbeat, the
// projector version and replay boundary, and the status columns other
// instances read with LoadStatuses. A single-segment projection uses
// segment 0.
//
// projection_dead_letters holds events skipped under
// [projection.Projection.OnError] on a non-stalling policy.
const Schema = `
CREATE TABLE IF NOT EXISTS projection_segments (
    name          TEXT        NOT NULL,
    segment       INT         NOT NULL,
    position      BIGINT      NOT NULL DEFAULT 0,
    version       INT         NOT NULL DEFAULT 0,
    replay_until  BIGINT      NOT NULL DEFAULT 0,
    phase         TEXT        NOT NULL DEFAULT 'starting',
    head          BIGINT      NOT NULL DEFAULT 0,
    last_error    TEXT,
    owner         TEXT,
    heartbeat     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (name, segment)
);

CREATE TABLE IF NOT EXISTS projection_dead_letters (
    id             BIGSERIAL   PRIMARY KEY,
    projection     TEXT        NOT NULL,
    segment        INT         NOT NULL,
    global_version BIGINT      NOT NULL,
    event_type     TEXT        NOT NULL,
    error          TEXT        NOT NULL,
    skipped_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_projection_dead_letters_projection
    ON projection_dead_letters (projection, global_version);
`

type txKey struct{}

// Tx returns the transaction a [projection.Runner] opened for the batch
// being processed under ctx. Handlers and reset functions write the read
// model through it, so their writes commit together with the checkpoint.
//
// Tx panics if ctx carries no transaction, which means it was called
// outside a projection that uses [Store]: a wiring mistake, not a
// condition to handle at run time.
func Tx(ctx context.Context) pgx.Tx {
	tx, ok := ctx.Value(txKey{}).(pgx.Tx)
	if !ok {
		panic("projection/postgres: Tx called outside a Store commit; wiring mistake")
	}
	return tx
}

// Store is the Postgres [projection.Store]. Create it with [NewStore].
type Store struct {
	db *pgxpool.Pool
}

// NewStore returns a Store that uses db. All operations open their own
// transactions against db except for a Commit's unit of work, which is the
// single transaction the handlers see through [Tx].
func NewStore(db *pgxpool.Pool) *Store {
	return &Store{db: db}
}

// Init creates the segment rows for name on first use, and verifies the
// count has not changed since.
func (s *Store) Init(ctx context.Context, name string, segments int) error {
	// Count existing rows; if none, insert; if some, verify the count.
	var count int
	err := s.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM projection_segments WHERE name = $1`, name,
	).Scan(&count)
	if err != nil {
		return fmt.Errorf("projection/postgres: count segments: %w", err)
	}
	if count == 0 {
		return s.insertSegments(ctx, name, segments)
	}
	if count != segments {
		return projection.ErrSegmentsChanged
	}
	return nil
}

func (s *Store) insertSegments(ctx context.Context, name string, segments int) error {
	batch := &pgx.Batch{}
	for i := 0; i < segments; i++ {
		batch.Queue(
			`INSERT INTO projection_segments (name, segment) VALUES ($1, $2)
			 ON CONFLICT (name, segment) DO NOTHING`,
			name, i,
		)
	}
	br := s.db.SendBatch(ctx, batch)
	defer br.Close()
	for i := 0; i < segments; i++ {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("projection/postgres: init segment %d: %w", i, err)
		}
	}
	return nil
}

// Claim takes segment for owner if its row is unowned, already ours, or
// its heartbeat is older than ttl. On success it refreshes the heartbeat
// and returns the row's checkpoint. See [projection.Store].
func (s *Store) Claim(ctx context.Context, name string, segment int, owner string, ttl time.Duration) (projection.Checkpoint, bool, error) {
	var cp projection.Checkpoint
	err := s.db.QueryRow(ctx, `
		UPDATE projection_segments
		SET owner = $3, heartbeat = now()
		WHERE name = $1 AND segment = $2
		  AND (owner IS NULL OR owner = $3 OR heartbeat < now() - make_interval(secs => $4))
		RETURNING position, version, replay_until`,
		name, segment, owner, ttl.Seconds(),
	).Scan(&cp.Position, &cp.Version, &cp.ReplayUntil)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return projection.Checkpoint{}, false, nil
		}
		return projection.Checkpoint{}, false, fmt.Errorf("projection/postgres: claim: %w", err)
	}
	return cp, true, nil
}

// Commit runs fn inside a transaction and, if fn returns nil and the owner
// still holds the segment, stores cp and status atomically. If the claim
// has been lost (another instance took over, or Release was called),
// fn's writes are rolled back and [projection.ErrClaimLost] is returned.
func (s *Store) Commit(ctx context.Context, name string, segment int, owner string, cp projection.Checkpoint, status projection.Status, fn func(ctx context.Context) error) error {
	return s.runTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// Pre-check and lock the row for the duration of the batch, so a
		// concurrent reclaim waits for us to commit or roll back.
		var currentOwner *string
		err := tx.QueryRow(ctx,
			`SELECT owner FROM projection_segments
			 WHERE name = $1 AND segment = $2 FOR UPDATE`,
			name, segment,
		).Scan(&currentOwner)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return projection.ErrClaimLost
			}
			return err
		}
		if currentOwner == nil || *currentOwner != owner {
			return projection.ErrClaimLost
		}

		if err := fn(ctx); err != nil {
			return err
		}

		var lastErr any
		if status.LastError != "" {
			lastErr = status.LastError
		}
		_, err = tx.Exec(ctx, `
			UPDATE projection_segments
			SET position = $3, version = $4, replay_until = $5,
			    phase = $6, head = $7, last_error = $8, heartbeat = now()
			WHERE name = $1 AND segment = $2 AND owner = $9`,
			name, segment,
			cp.Position, cp.Version, cp.ReplayUntil,
			status.Phase.String(), status.Head, lastErr,
			owner,
		)
		return err
	})
}

// Reset wipes read-model state via wipe and resets the segment's
// checkpoint, in one transaction. See [projection.Store].
func (s *Store) Reset(ctx context.Context, name string, segment int, owner string, newVersion int, wipe func(ctx context.Context) error) error {
	return s.runTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var currentOwner *string
		var oldPos uint64
		err := tx.QueryRow(ctx,
			`SELECT owner, position FROM projection_segments
			 WHERE name = $1 AND segment = $2 FOR UPDATE`,
			name, segment,
		).Scan(&currentOwner, &oldPos)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return projection.ErrClaimLost
			}
			return err
		}
		if currentOwner == nil || *currentOwner != owner {
			return projection.ErrClaimLost
		}

		if err := wipe(ctx); err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `
			UPDATE projection_segments
			SET position = 0, version = $3, replay_until = $4,
			    phase = 'starting', head = 0, last_error = NULL,
			    heartbeat = now()
			WHERE name = $1 AND segment = $2 AND owner = $5`,
			name, segment, newVersion, oldPos, owner,
		)
		return err
	})
}

// Release gives the segment up so another instance can take it at once.
func (s *Store) Release(ctx context.Context, name string, segment int, owner string) error {
	_, err := s.db.Exec(ctx, `
		UPDATE projection_segments
		SET owner = NULL
		WHERE name = $1 AND segment = $2 AND owner = $3`,
		name, segment, owner,
	)
	if err != nil {
		return fmt.Errorf("projection/postgres: release: %w", err)
	}
	return nil
}

// SaveStatus publishes status for a segment the caller still owns. Used
// between batches to update Phase without a Commit.
func (s *Store) SaveStatus(ctx context.Context, name string, segment int, owner string, st projection.Status) error {
	var lastErr any
	if st.LastError != "" {
		lastErr = st.LastError
	}
	_, err := s.db.Exec(ctx, `
		UPDATE projection_segments
		SET phase = $3, head = $4, last_error = $5, heartbeat = now()
		WHERE name = $1 AND segment = $2 AND owner = $6`,
		name, segment, st.Phase.String(), st.Head, lastErr, owner,
	)
	return err
}

// LoadStatuses returns the status of every segment of name.
func (s *Store) LoadStatuses(ctx context.Context, name string) ([]projection.Status, error) {
	rows, err := s.db.Query(ctx, `
		SELECT segment, phase, position, version, head, last_error, owner,
		       EXTRACT(EPOCH FROM (now() - heartbeat))
		FROM projection_segments
		WHERE name = $1
		ORDER BY segment`,
		name,
	)
	if err != nil {
		return nil, fmt.Errorf("projection/postgres: load statuses: %w", err)
	}
	defer rows.Close()

	var out []projection.Status
	for rows.Next() {
		var (
			seg        int
			phase      string
			position   uint64
			version    int
			head       uint64
			lastErr    *string
			owner      *string
			ageSeconds float64
		)
		if err := rows.Scan(&seg, &phase, &position, &version, &head, &lastErr, &owner, &ageSeconds); err != nil {
			return nil, err
		}
		st := projection.Status{
			Phase:    parsePhase(phase),
			Position: position,
			Head:     head,
			Version:  version,
			Segment:  seg,
			Age:      time.Duration(ageSeconds * float64(time.Second)),
		}
		if lastErr != nil {
			st.LastError = *lastErr
		}
		if owner != nil {
			st.Owner = *owner
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// SaveDeadLetter records a skipped event. It is called inside the Commit
// that moves the checkpoint past the event.
func (s *Store) SaveDeadLetter(ctx context.Context, name string, segment int, position uint64, eventType string, cause error) error {
	tx, ok := ctx.Value(txKey{}).(pgx.Tx)
	if !ok {
		return fmt.Errorf("projection/postgres: SaveDeadLetter called outside a Store commit")
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO projection_dead_letters
		(projection, segment, global_version, event_type, error)
		VALUES ($1, $2, $3, $4, $5)`,
		name, segment, position, eventType, cause.Error(),
	)
	return err
}

// runTx runs fn in a new transaction. On error, fn's work is rolled back.
func (s *Store) runTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	ctxWithTx := context.WithValue(ctx, txKey{}, tx)
	if err := fn(ctxWithTx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func parsePhase(s string) projection.Phase {
	switch s {
	case "starting":
		return projection.Starting
	case "standby":
		return projection.Standby
	case "catching_up":
		return projection.CatchingUp
	case "live":
		return projection.Live
	case "stalled":
		return projection.Stalled
	}
	return projection.Starting
}

// Store implements both projection.Store and projection.DeadLetterStore.
var (
	_ projection.Store           = (*Store)(nil)
	_ projection.DeadLetterStore = (*Store)(nil)
)
