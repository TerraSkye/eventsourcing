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

type txKey struct{}

// Tx returns the transaction a [projection.Runner] opened for the batch
// being processed under ctx. Handlers and reset functions write the read
// model through it, so their writes commit together with the checkpoint.
//
// Tx panics if ctx carries no transaction, which means it was called
// outside a projection that uses [ProjectionStore]: a wiring mistake, not a
// condition to handle at run time.
func Tx(ctx context.Context) pgx.Tx {
	tx, ok := ctx.Value(txKey{}).(pgx.Tx)
	if !ok {
		panic("postgres: Tx called outside a ProjectionStore commit; wiring mistake")
	}
	return tx
}

// ProjectionStore is the Postgres [projection.Store]. Create it with [NewProjectionStore].
//
// Commit and Reset lock the segment's row for the whole unit of work, so
// a Claim by another instance waits for the batch to finish instead of
// taking the segment over halfway. Heartbeats use clock_timestamp(), the
// time of the write, rather than now(), the start of the transaction, so a
// batch that ran longer than the claim's ttl still leaves a fresh
// heartbeat behind and the waiting Claim is refused.
type ProjectionStore struct {
	db *pgxpool.Pool
}

// NewProjectionStore returns a ProjectionStore that uses db. All operations open their own
// transactions against db except for a Commit's unit of work, which is the
// single transaction the handlers see through [Tx].
func NewProjectionStore(db *pgxpool.Pool) *ProjectionStore {
	return &ProjectionStore{db: db}
}

// Declare implements [projection.Store]: it creates the segment rows for name
// on first use, and verifies the count has not changed since.
func (s *ProjectionStore) Declare(ctx context.Context, name string, segments int, replayUntil uint64) error {
	// Count existing rows; if none, insert; if some, verify the count.
	var count int
	err := s.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM projection_segments WHERE name = $1`, name,
	).Scan(&count)
	if err != nil {
		return fmt.Errorf("postgres: declare %q: count segments: %w", name, err)
	}
	if count == 0 {
		return s.insertSegments(ctx, name, segments, replayUntil)
	}
	if count != segments {
		return projection.ErrSegmentsChanged
	}
	return nil
}

func (s *ProjectionStore) insertSegments(ctx context.Context, name string, segments int, replayUntil uint64) error {
	batch := &pgx.Batch{}
	for i := 0; i < segments; i++ {
		batch.Queue(
			`INSERT INTO projection_segments (name, segment, replay_until) VALUES ($1, $2, $3)
			 ON CONFLICT (name, segment) DO NOTHING`,
			name, i, replayUntil,
		)
	}
	br := s.db.SendBatch(ctx, batch)
	defer br.Close()
	for i := 0; i < segments; i++ {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("postgres: declare %q segment %d: %w", name, i, err)
		}
	}
	return nil
}

// Claim implements [projection.Store].
//
// A refresh of the current claim keeps its epoch; a takeover raises it.
// On the right-hand side of SET, owner and epoch are the row's values
// before the update.
func (s *ProjectionStore) Claim(ctx context.Context, l projection.Lease, ttl time.Duration) (projection.Lease, projection.Checkpoint, bool, error) {
	var cp projection.Checkpoint
	err := s.db.QueryRow(ctx, `
		UPDATE projection_segments
		SET owner = $3, heartbeat = clock_timestamp(),
		    epoch = CASE WHEN owner = $3 AND epoch = $5 THEN epoch ELSE epoch + 1 END
		WHERE name = $1 AND segment = $2
		  AND ((owner = $3 AND epoch = $5)
		       OR owner IS NULL
		       OR heartbeat < clock_timestamp() - make_interval(secs => $4))
		RETURNING epoch, position, version, replay_until, fingerprint`,
		l.Name, l.Segment, l.Owner, ttl.Seconds(), l.Epoch,
	).Scan(&l.Epoch, &cp.Position, &cp.Version, &cp.ReplayUntil, &cp.Fingerprint)
	if errors.Is(err, pgx.ErrNoRows) {
		return projection.Lease{}, projection.Checkpoint{}, false, nil
	}
	if err != nil {
		return projection.Lease{}, projection.Checkpoint{}, false, fmt.Errorf("postgres: claim %q segment %d: %w", l.Name, l.Segment, err)
	}
	return l, cp, true, nil
}

// Commit implements [projection.Store]. It runs fn in a transaction that
// first locks the segment's row, so a Claim by another instance waits for
// the batch instead of taking the segment over halfway.
func (s *ProjectionStore) Commit(ctx context.Context, l projection.Lease, cp projection.Checkpoint, status projection.Status, fn func(ctx context.Context) error) error {
	return s.runTx(ctx, "commit", l, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := lockOwned(ctx, tx, l); err != nil {
			return err
		}
		if err := fn(ctx); err != nil {
			return err
		}
		var lastErr any
		if status.LastError != "" {
			lastErr = status.LastError
		}
		_, err := tx.Exec(ctx, `
			UPDATE projection_segments
			SET position = $3, version = $4, replay_until = $5, fingerprint = $11,
			    phase = $6, head = $7, last_error = $8, heartbeat = clock_timestamp()
			WHERE name = $1 AND segment = $2 AND owner = $9 AND epoch = $10`,
			l.Name, l.Segment,
			cp.Position, cp.Version, cp.ReplayUntil,
			status.Phase.String(), status.Head, lastErr,
			l.Owner, l.Epoch, cp.Fingerprint,
		)
		if err != nil {
			return fmt.Errorf("postgres: store checkpoint of %q segment %d: %w", l.Name, l.Segment, err)
		}
		return nil
	})
}

// Reset implements [projection.Store], in one transaction that first locks
// every segment row of the projection in segment order: Commits in
// progress finish first, and concurrent Resets cannot deadlock. On the
// right-hand side of SET, position and owner are the row's values before
// the update.
func (s *ProjectionStore) Reset(ctx context.Context, l projection.Lease, newVersion int, wipe func(ctx context.Context) error) error {
	return s.runTx(ctx, "reset", l, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT segment, owner, epoch FROM projection_segments
			 WHERE name = $1 ORDER BY segment FOR UPDATE`,
			l.Name,
		)
		if err != nil {
			return fmt.Errorf("postgres: lock segments of %q: %w", l.Name, err)
		}
		held := false
		for rows.Next() {
			var (
				segment int
				owner   *string
				epoch   uint64
			)
			if err := rows.Scan(&segment, &owner, &epoch); err != nil {
				rows.Close()
				return fmt.Errorf("postgres: lock segments of %q: %w", l.Name, err)
			}
			if segment == l.Segment && owner != nil && *owner == l.Owner && epoch == l.Epoch {
				held = true
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("postgres: lock segments of %q: %w", l.Name, err)
		}
		if !held {
			return projection.ErrClaimLost
		}

		if err := wipe(ctx); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			UPDATE projection_segments
			SET replay_until = position, position = 0, version = $2, fingerprint = '',
			    head = 0, last_error = NULL,
			    phase = CASE WHEN segment = $3 THEN 'starting' ELSE 'standby' END,
			    owner = CASE WHEN segment = $3 THEN owner END,
			    heartbeat = CASE WHEN segment = $3 THEN clock_timestamp() ELSE heartbeat END
			WHERE name = $1`,
			l.Name, newVersion, l.Segment,
		)
		if err != nil {
			return fmt.Errorf("postgres: reset checkpoints of %q: %w", l.Name, err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM projection_dead_letters WHERE projection = $1`, l.Name); err != nil {
			return fmt.Errorf("postgres: clear dead letters of %q: %w", l.Name, err)
		}
		return nil
	})
}

// lockOwned locks l's segment row for the rest of tx and returns its
// position, or returns [projection.ErrClaimLost] when l is not the
// segment's current claim.
func lockOwned(ctx context.Context, tx pgx.Tx, l projection.Lease) (uint64, error) {
	var (
		owner    *string
		epoch    uint64
		position uint64
	)
	err := tx.QueryRow(ctx,
		`SELECT owner, epoch, position FROM projection_segments
		 WHERE name = $1 AND segment = $2 FOR UPDATE`,
		l.Name, l.Segment,
	).Scan(&owner, &epoch, &position)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, projection.ErrClaimLost
	}
	if err != nil {
		return 0, fmt.Errorf("postgres: lock %q segment %d: %w", l.Name, l.Segment, err)
	}
	if owner == nil || *owner != l.Owner || epoch != l.Epoch {
		return 0, projection.ErrClaimLost
	}
	return position, nil
}

// Release implements [projection.Store].
func (s *ProjectionStore) Release(ctx context.Context, l projection.Lease) error {
	_, err := s.db.Exec(ctx, `
		UPDATE projection_segments
		SET owner = NULL, phase = 'standby'
		WHERE name = $1 AND segment = $2 AND owner = $3 AND epoch = $4`,
		l.Name, l.Segment, l.Owner, l.Epoch,
	)
	if err != nil {
		return fmt.Errorf("postgres: release %q segment %d: %w", l.Name, l.Segment, err)
	}
	return nil
}

// SaveStatus implements [projection.Store].
func (s *ProjectionStore) SaveStatus(ctx context.Context, l projection.Lease, status projection.Status) error {
	var lastErr any
	if status.LastError != "" {
		lastErr = status.LastError
	}
	tag, err := s.db.Exec(ctx, `
		UPDATE projection_segments
		SET phase = $3, head = $4, last_error = $5, heartbeat = clock_timestamp()
		WHERE name = $1 AND segment = $2 AND owner = $6 AND epoch = $7`,
		l.Name, l.Segment, status.Phase.String(), status.Head, lastErr, l.Owner, l.Epoch,
	)
	if err != nil {
		return fmt.Errorf("postgres: save status of %q segment %d: %w", l.Name, l.Segment, err)
	}
	if tag.RowsAffected() == 0 {
		return projection.ErrClaimLost
	}
	return nil
}

// LoadStatuses returns the status of every segment of name.
func (s *ProjectionStore) LoadStatuses(ctx context.Context, name string) ([]projection.Status, error) {
	rows, err := s.db.Query(ctx, `
		SELECT segment, phase, position, version, head, last_error, owner,
		       EXTRACT(EPOCH FROM (clock_timestamp() - heartbeat))
		FROM projection_segments
		WHERE name = $1
		ORDER BY segment`,
		name,
	)
	if err != nil {
		return nil, fmt.Errorf("postgres: load statuses of %q: %w", name, err)
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
			return nil, fmt.Errorf("postgres: load statuses of %q: %w", name, err)
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
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: load statuses of %q: %w", name, err)
	}
	return out, nil
}

// SaveDeadLetter implements [projection.DeadLetterStore]. It is called
// inside the Commit that moves the checkpoint past the event.
func (s *ProjectionStore) SaveDeadLetter(ctx context.Context, l projection.Lease, d projection.DeadLetter) error {
	tx, ok := ctx.Value(txKey{}).(pgx.Tx)
	if !ok {
		return errors.New("postgres: SaveDeadLetter called outside a ProjectionStore commit")
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO projection_dead_letters
		(projection, segment, global_position, stream_id, event_type, error)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		l.Name, l.Segment, d.Position, d.StreamID, d.EventType, d.Error,
	)
	if err != nil {
		return fmt.Errorf("postgres: save dead letter of %q segment %d at %d: %w", l.Name, l.Segment, d.Position, err)
	}
	return nil
}

// LoadDeadLetters implements [projection.DeadLetterStore].
func (s *ProjectionStore) LoadDeadLetters(ctx context.Context, name string, segment int) ([]projection.DeadLetter, error) {
	rows, err := s.db.Query(ctx, `
		SELECT global_position, stream_id, event_type, error
		FROM projection_dead_letters
		WHERE projection = $1 AND segment = $2
		ORDER BY global_position`,
		name, segment,
	)
	if err != nil {
		return nil, fmt.Errorf("postgres: load dead letters of %q segment %d: %w", name, segment, err)
	}
	defer rows.Close()
	var out []projection.DeadLetter
	for rows.Next() {
		var d projection.DeadLetter
		if err := rows.Scan(&d.Position, &d.StreamID, &d.EventType, &d.Error); err != nil {
			return nil, fmt.Errorf("postgres: load dead letters of %q segment %d: %w", name, segment, err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: load dead letters of %q segment %d: %w", name, segment, err)
	}
	return out, nil
}

// DeleteDeadLetter implements [projection.DeadLetterStore]. It is called
// inside the Commit that applies the event again.
func (s *ProjectionStore) DeleteDeadLetter(ctx context.Context, l projection.Lease, position uint64) error {
	tx, ok := ctx.Value(txKey{}).(pgx.Tx)
	if !ok {
		return errors.New("postgres: DeleteDeadLetter called outside a ProjectionStore commit")
	}
	_, err := tx.Exec(ctx, `
		DELETE FROM projection_dead_letters
		WHERE projection = $1 AND segment = $2 AND global_position = $3`,
		l.Name, l.Segment, position,
	)
	if err != nil {
		return fmt.Errorf("postgres: delete dead letter of %q segment %d at %d: %w", l.Name, l.Segment, position, err)
	}
	return nil
}

// runTx runs fn in a new transaction, committing it if fn returns nil and
// rolling it back otherwise. fn's own errors are returned unchanged, so
// [projection.ErrClaimLost] and handler errors reach the runner as they
// are; op names the call in errors from beginning or committing.
func (s *ProjectionStore) runTx(ctx context.Context, op string, l projection.Lease, fn func(ctx context.Context, tx pgx.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("postgres: %s %q segment %d: begin: %w", op, l.Name, l.Segment, err)
	}
	// Rollback after Commit is a no-op, and a failed rollback leaves
	// nothing to clean up: the transaction ends with the connection.
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(context.WithValue(ctx, txKey{}, tx), tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: %s %q segment %d: commit: %w", op, l.Name, l.Segment, err)
	}
	return nil
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

// ProjectionStore implements both projection.Store and projection.DeadLetterStore.
var (
	_ projection.Store           = (*ProjectionStore)(nil)
	_ projection.DeadLetterStore = (*ProjectionStore)(nil)
)
