package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/terraskye/eventsourcing/projection"
)

// ProjectionStore implements both projection.Store and
// projection.DeadLetterStore.
var (
	_ projection.Store           = (*ProjectionStore)(nil)
	_ projection.DeadLetterStore = (*ProjectionStore)(nil)
)

// nowMS is SQLite's current time as Unix milliseconds. It is evaluated per
// statement, so a heartbeat written at the end of a long batch is the time
// of that write.
const nowMS = `CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER)`

type txKey struct{}

// Tx returns the transaction a [projection.Runner] opened for the batch
// being processed under ctx. Handlers and reset functions write the read
// model through it, so their writes commit together with the checkpoint.
//
// A handler must not use the *sql.DB itself: with the one connection
// [Open] allows, the batch's transaction holds it; see the package docs.
//
// Tx panics if ctx carries no transaction, which means it was called
// outside a projection that uses [ProjectionStore]: a wiring mistake, not a
// condition to handle at run time.
func Tx(ctx context.Context) *sql.Tx {
	tx, ok := ctx.Value(txKey{}).(*sql.Tx)
	if !ok {
		panic("sqlite: Tx called outside a ProjectionStore commit; wiring mistake")
	}
	return tx
}

// ProjectionStore is the SQLite [projection.Store]. Create it with
// [NewProjectionStore].
//
// Each Commit and Reset runs in one transaction, so the read model's writes
// and the checkpoint move land together, and every call that takes a
// [projection.Lease] is fenced by a conditional update on owner and epoch.
type ProjectionStore struct {
	db *sql.DB
}

// NewProjectionStore returns a ProjectionStore on db, which must hold the
// tables [Schema] creates and the read model's tables. Open db with [Open].
func NewProjectionStore(db *sql.DB) *ProjectionStore {
	return &ProjectionStore{db: db}
}

// Declare implements [projection.Store]: it creates the segment rows for
// name on first use, and verifies the count has not changed since.
func (s *ProjectionStore) Declare(ctx context.Context, name string, segments int, replayUntil uint64) error {
	return s.runTx(ctx, "declare", name, func(ctx context.Context, tx *sql.Tx) error {
		var count int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM projection_segments WHERE name = ?`, name,
		).Scan(&count); err != nil {
			return fmt.Errorf("sqlite: declare %q: count segments: %w", name, err)
		}
		if count != 0 {
			if count != segments {
				return projection.ErrSegmentsChanged
			}
			return nil
		}
		for i := range segments {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO projection_segments (name, segment, replay_until, heartbeat)
				VALUES (?, ?, ?, `+nowMS+`)`,
				name, i, int64(replayUntil),
			); err != nil {
				return fmt.Errorf("sqlite: declare %q segment %d: %w", name, i, err)
			}
		}
		return nil
	})
}

// Claim implements [projection.Store]. A refresh of the current claim
// keeps its epoch; a takeover raises it. On the right-hand side of SET,
// owner and epoch are the row's values before the update.
func (s *ProjectionStore) Claim(ctx context.Context, l projection.Lease, ttl time.Duration) (projection.Lease, projection.Checkpoint, bool, error) {
	var (
		cp          projection.Checkpoint
		epoch       int64
		position    int64
		replayUntil int64
	)
	err := s.db.QueryRowContext(ctx, `
		UPDATE projection_segments
		SET owner = ?3, heartbeat = `+nowMS+`,
		    epoch = CASE WHEN owner = ?3 AND epoch = ?5 THEN epoch ELSE epoch + 1 END
		WHERE name = ?1 AND segment = ?2
		  AND ((owner = ?3 AND epoch = ?5)
		       OR owner IS NULL
		       OR heartbeat < `+nowMS+` - ?4)
		RETURNING epoch, position, version, replay_until`,
		l.Name, l.Segment, l.Owner, ttl.Milliseconds(), int64(l.Epoch),
	).Scan(&epoch, &position, &cp.Version, &replayUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return projection.Lease{}, projection.Checkpoint{}, false, nil
	}
	if err != nil {
		return projection.Lease{}, projection.Checkpoint{}, false, fmt.Errorf("sqlite: claim %q segment %d: %w", l.Name, l.Segment, err)
	}
	l.Epoch = uint64(epoch)
	cp.Position = uint64(position)
	cp.ReplayUntil = uint64(replayUntil)
	return l, cp, true, nil
}

// Commit implements [projection.Store]. It runs fn and the checkpoint
// update in one transaction; the update only applies while l is the
// current claim, and otherwise the transaction rolls back.
func (s *ProjectionStore) Commit(ctx context.Context, l projection.Lease, cp projection.Checkpoint, status projection.Status, fn func(ctx context.Context) error) error {
	return s.runTx(ctx, "commit", l.Name, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := ownedPosition(ctx, tx, l); err != nil {
			return err
		}
		if err := fn(ctx); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE projection_segments
			SET position = ?, version = ?, replay_until = ?,
			    phase = ?, head = ?, last_error = ?, heartbeat = `+nowMS+`
			WHERE name = ? AND segment = ? AND owner = ? AND epoch = ?`,
			int64(cp.Position), cp.Version, int64(cp.ReplayUntil),
			status.Phase.String(), int64(status.Head), nullString(status.LastError),
			l.Name, l.Segment, l.Owner, int64(l.Epoch),
		)
		if err != nil {
			return fmt.Errorf("sqlite: store checkpoint of %q segment %d: %w", l.Name, l.Segment, err)
		}
		return claimHeld(res)
	})
}

// Reset implements [projection.Store], in one transaction fenced like
// Commit.
func (s *ProjectionStore) Reset(ctx context.Context, l projection.Lease, newVersion int, wipe func(ctx context.Context) error) error {
	return s.runTx(ctx, "reset", l.Name, func(ctx context.Context, tx *sql.Tx) error {
		oldPos, err := ownedPosition(ctx, tx, l)
		if err != nil {
			return err
		}
		if err := wipe(ctx); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE projection_segments
			SET position = 0, version = ?, replay_until = ?,
			    phase = 'starting', head = 0, last_error = NULL,
			    heartbeat = `+nowMS+`
			WHERE name = ? AND segment = ? AND owner = ? AND epoch = ?`,
			newVersion, int64(oldPos), l.Name, l.Segment, l.Owner, int64(l.Epoch),
		)
		if err != nil {
			return fmt.Errorf("sqlite: reset checkpoint of %q segment %d: %w", l.Name, l.Segment, err)
		}
		return claimHeld(res)
	})
}

// ownedPosition returns the position of l's segment, or
// [projection.ErrClaimLost] when l is not the segment's current claim.
func ownedPosition(ctx context.Context, tx *sql.Tx, l projection.Lease) (uint64, error) {
	var position int64
	err := tx.QueryRowContext(ctx, `
		SELECT position FROM projection_segments
		WHERE name = ? AND segment = ? AND owner = ? AND epoch = ?`,
		l.Name, l.Segment, l.Owner, int64(l.Epoch),
	).Scan(&position)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, projection.ErrClaimLost
	}
	if err != nil {
		return 0, fmt.Errorf("sqlite: read %q segment %d: %w", l.Name, l.Segment, err)
	}
	return uint64(position), nil
}

// Release implements [projection.Store].
func (s *ProjectionStore) Release(ctx context.Context, l projection.Lease) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE projection_segments
		SET owner = NULL, phase = 'standby'
		WHERE name = ? AND segment = ? AND owner = ? AND epoch = ?`,
		l.Name, l.Segment, l.Owner, int64(l.Epoch),
	)
	if err != nil {
		return fmt.Errorf("sqlite: release %q segment %d: %w", l.Name, l.Segment, err)
	}
	return nil
}

// SaveStatus implements [projection.Store].
func (s *ProjectionStore) SaveStatus(ctx context.Context, l projection.Lease, status projection.Status) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE projection_segments
		SET phase = ?, head = ?, last_error = ?, heartbeat = `+nowMS+`
		WHERE name = ? AND segment = ? AND owner = ? AND epoch = ?`,
		status.Phase.String(), int64(status.Head), nullString(status.LastError),
		l.Name, l.Segment, l.Owner, int64(l.Epoch),
	)
	if err != nil {
		return fmt.Errorf("sqlite: save status of %q segment %d: %w", l.Name, l.Segment, err)
	}
	return claimHeld(res)
}

// LoadStatuses implements [projection.Store].
func (s *ProjectionStore) LoadStatuses(ctx context.Context, name string) ([]projection.Status, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT segment, phase, position, version, head, last_error, owner,
		       `+nowMS+` - heartbeat
		FROM projection_segments
		WHERE name = ?
		ORDER BY segment`,
		name,
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: load statuses of %q: %w", name, err)
	}
	defer rows.Close()

	var out []projection.Status
	for rows.Next() {
		var (
			st       projection.Status
			phase    string
			position int64
			head     int64
			lastErr  sql.NullString
			owner    sql.NullString
			ageMS    int64
		)
		if err := rows.Scan(&st.Segment, &phase, &position, &st.Version, &head, &lastErr, &owner, &ageMS); err != nil {
			return nil, fmt.Errorf("sqlite: load statuses of %q: %w", name, err)
		}
		st.Phase = parsePhase(phase)
		st.Position = uint64(position)
		st.Head = uint64(head)
		st.LastError = lastErr.String
		st.Owner = owner.String
		st.Age = time.Duration(max(ageMS, 0)) * time.Millisecond
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: load statuses of %q: %w", name, err)
	}
	return out, nil
}

// SaveDeadLetter implements [projection.DeadLetterStore]. It is called
// inside the Commit that moves the checkpoint past the event.
func (s *ProjectionStore) SaveDeadLetter(ctx context.Context, l projection.Lease, position uint64, eventType string, cause error) error {
	tx, ok := ctx.Value(txKey{}).(*sql.Tx)
	if !ok {
		return errors.New("sqlite: SaveDeadLetter called outside a ProjectionStore commit")
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO projection_dead_letters
		(projection, segment, global_position, event_type, error, skipped_at)
		VALUES (?, ?, ?, ?, ?, `+nowMS+`)`,
		l.Name, l.Segment, int64(position), eventType, cause.Error(),
	)
	if err != nil {
		return fmt.Errorf("sqlite: save dead letter of %q segment %d at %d: %w", l.Name, l.Segment, position, err)
	}
	return nil
}

// runTx runs fn in a new transaction, committing it if fn returns nil and
// rolling it back otherwise. fn's own errors are returned unchanged, so
// [projection.ErrClaimLost] and handler errors reach the runner as they
// are; op names the call in errors from beginning or committing.
func (s *ProjectionStore) runTx(ctx context.Context, op, name string, fn func(ctx context.Context, tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: %s %q: begin: %w", op, name, err)
	}
	// Rollback after Commit is a no-op.
	defer func() { _ = tx.Rollback() }()
	if err := fn(context.WithValue(ctx, txKey{}, tx), tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: %s %q: commit: %w", op, name, err)
	}
	return nil
}

// claimHeld returns [projection.ErrClaimLost] when a fenced update changed
// no row.
func claimHeld(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return projection.ErrClaimLost
	}
	return nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func parsePhase(s string) projection.Phase {
	switch s {
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
