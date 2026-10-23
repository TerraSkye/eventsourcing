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
		RETURNING epoch, position, version, replay_until, fingerprint`,
		l.Name, l.Segment, l.Owner, ttl.Milliseconds(), int64(l.Epoch),
	).Scan(&epoch, &position, &cp.Version, &replayUntil, &cp.Fingerprint)
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
			SET position = ?, version = ?, replay_until = ?, fingerprint = ?,
			    phase = ?, head = ?, last_error = ?, heartbeat = `+nowMS+`
			WHERE name = ? AND segment = ? AND owner = ? AND epoch = ?`,
			int64(cp.Position), cp.Version, int64(cp.ReplayUntil), cp.Fingerprint,
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
// Commit. The transaction holds the database's write lock, so Commits run
// before or after it, never during. On the right-hand side of SET,
// position and owner are the row's values before the update.
func (s *ProjectionStore) Reset(ctx context.Context, l projection.Lease, newVersion int, wipe func(ctx context.Context) error) error {
	return s.runTx(ctx, "reset", l.Name, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := ownedPosition(ctx, tx, l); err != nil {
			return err
		}
		if err := wipe(ctx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE projection_segments
			SET replay_until = MAX(replay_until, position), position = 0, version = ?2, fingerprint = '',
			    head = 0, last_error = NULL,
			    phase = CASE WHEN segment = ?3 THEN 'starting' ELSE 'standby' END,
			    owner = CASE WHEN segment = ?3 THEN owner END,
			    heartbeat = CASE WHEN segment = ?3 THEN `+nowMS+` ELSE heartbeat END
			WHERE name = ?1`,
			l.Name, newVersion, l.Segment,
		); err != nil {
			return fmt.Errorf("sqlite: reset checkpoints of %q: %w", l.Name, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM projection_dead_letters WHERE projection = ?`, l.Name); err != nil {
			return fmt.Errorf("sqlite: clear dead letters of %q: %w", l.Name, err)
		}
		// Every rebuild requested so far is served by this one. The
		// transaction holds the write lock, so a request runs before or
		// after it, never in between.
		if _, err := tx.ExecContext(ctx, `UPDATE projection_rebuilds SET done = requested WHERE name = ?`, l.Name); err != nil {
			return fmt.Errorf("sqlite: mark rebuild requests of %q done: %w", l.Name, err)
		}
		return nil
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
		SELECT s.segment, s.phase, s.position, s.version, s.head, s.last_error, s.owner,
		       `+nowMS+` - s.heartbeat,
		       s.replay_until, COALESCE(r.requested > r.done, 0)
		FROM projection_segments s
		LEFT JOIN projection_rebuilds r ON r.name = s.name
		WHERE s.name = ?
		ORDER BY s.segment`,
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
			replay   int64
			pending  bool
		)
		if err := rows.Scan(&st.Segment, &phase, &position, &st.Version, &head, &lastErr, &owner, &ageMS, &replay, &pending); err != nil {
			return nil, fmt.Errorf("sqlite: load statuses of %q: %w", name, err)
		}
		st.Phase = parsePhase(phase)
		st.Position = uint64(position)
		st.Head = uint64(head)
		st.LastError = lastErr.String
		st.Owner = owner.String
		st.Age = time.Duration(max(ageMS, 0)) * time.Millisecond
		st.ReplayUntil = uint64(replay)
		st.RebuildRequested = pending
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: load statuses of %q: %w", name, err)
	}
	return out, nil
}

// Heartbeat implements [projection.Store], in one transaction that upserts
// owner's row, deletes the expired rows of other instances, and reads the
// live ones with the number of segments each one claims.
func (s *ProjectionStore) Heartbeat(ctx context.Context, name, owner string, version int, ttl time.Duration) ([]projection.Instance, error) {
	if ttl == 0 {
		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM projection_instances WHERE name = ? AND owner = ?`, name, owner,
		); err != nil {
			return nil, fmt.Errorf("sqlite: leave %q as %q: %w", name, owner, err)
		}
		return nil, nil
	}
	var out []projection.Instance
	err := s.runTx(ctx, "heartbeat", name, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO projection_instances (name, owner, version, heartbeat)
			VALUES (?1, ?2, ?3, `+nowMS+`)
			ON CONFLICT (name, owner) DO UPDATE SET version = excluded.version, heartbeat = excluded.heartbeat`,
			name, owner, version,
		); err != nil {
			return fmt.Errorf("sqlite: heartbeat %q as %q: %w", name, owner, err)
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM projection_instances WHERE name = ? AND heartbeat < `+nowMS+` - ?`,
			name, ttl.Milliseconds(),
		); err != nil {
			return fmt.Errorf("sqlite: heartbeat %q as %q: delete expired instances: %w", name, owner, err)
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT m.owner, m.version, `+nowMS+` - m.heartbeat,
			       (SELECT count(*) FROM projection_segments s WHERE s.name = m.name AND s.owner = m.owner)
			FROM projection_instances m
			WHERE m.name = ?
			ORDER BY m.owner`,
			name,
		)
		if err != nil {
			return fmt.Errorf("sqlite: heartbeat %q as %q: load instances: %w", name, owner, err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				m     projection.Instance
				ageMS int64
			)
			if err := rows.Scan(&m.Owner, &m.Version, &ageMS, &m.Segments); err != nil {
				return fmt.Errorf("sqlite: heartbeat %q as %q: load instances: %w", name, owner, err)
			}
			m.Age = time.Duration(max(ageMS, 0)) * time.Millisecond
			out = append(out, m)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RequestRebuild implements [projection.Store].
func (s *ProjectionStore) RequestRebuild(ctx context.Context, name string) (uint64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO projection_rebuilds (name, requested)
		SELECT ?1, 1 WHERE EXISTS (SELECT 1 FROM projection_segments WHERE name = ?1)
		ON CONFLICT (name) DO UPDATE SET requested = requested + 1
		RETURNING requested`,
		name,
	).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, projection.ErrUnknownProjection
	}
	if err != nil {
		return 0, fmt.Errorf("sqlite: request rebuild of %q: %w", name, err)
	}
	return uint64(n), nil
}

// Rebuilds implements [projection.Store].
func (s *ProjectionStore) Rebuilds(ctx context.Context, name string) (requested, done uint64, err error) {
	var r, d int64
	err = s.db.QueryRowContext(ctx,
		`SELECT requested, done FROM projection_rebuilds WHERE name = ?`, name,
	).Scan(&r, &d)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("sqlite: read rebuild requests of %q: %w", name, err)
	}
	return uint64(r), uint64(d), nil
}

// SaveDeadLetter implements [projection.DeadLetterStore]. It is called
// inside the Commit that moves the checkpoint past the event.
func (s *ProjectionStore) SaveDeadLetter(ctx context.Context, l projection.Lease, d projection.DeadLetter) error {
	tx, ok := ctx.Value(txKey{}).(*sql.Tx)
	if !ok {
		return errors.New("sqlite: SaveDeadLetter called outside a ProjectionStore commit")
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO projection_dead_letters
		(projection, segment, global_position, stream_id, event_type, error, skipped_at)
		VALUES (?, ?, ?, ?, ?, ?, `+nowMS+`)`,
		l.Name, l.Segment, int64(d.Position), d.StreamID, d.EventType, d.Error,
	)
	if err != nil {
		return fmt.Errorf("sqlite: save dead letter of %q segment %d at %d: %w", l.Name, l.Segment, d.Position, err)
	}
	return nil
}

// LoadDeadLetters implements [projection.DeadLetterStore].
func (s *ProjectionStore) LoadDeadLetters(ctx context.Context, name string, segment int) ([]projection.DeadLetter, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT global_position, stream_id, event_type, error
		FROM projection_dead_letters
		WHERE projection = ? AND segment = ?
		ORDER BY global_position`,
		name, segment,
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: load dead letters of %q segment %d: %w", name, segment, err)
	}
	defer rows.Close()
	var out []projection.DeadLetter
	for rows.Next() {
		var (
			d        projection.DeadLetter
			position int64
		)
		if err := rows.Scan(&position, &d.StreamID, &d.EventType, &d.Error); err != nil {
			return nil, fmt.Errorf("sqlite: load dead letters of %q segment %d: %w", name, segment, err)
		}
		d.Position = uint64(position)
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: load dead letters of %q segment %d: %w", name, segment, err)
	}
	return out, nil
}

// DeleteDeadLetter implements [projection.DeadLetterStore]. It is called
// inside the Commit that applies the event again.
func (s *ProjectionStore) DeleteDeadLetter(ctx context.Context, l projection.Lease, position uint64) error {
	tx, ok := ctx.Value(txKey{}).(*sql.Tx)
	if !ok {
		return errors.New("sqlite: DeleteDeadLetter called outside a ProjectionStore commit")
	}
	_, err := tx.ExecContext(ctx, `
		DELETE FROM projection_dead_letters
		WHERE projection = ? AND segment = ? AND global_position = ?`,
		l.Name, l.Segment, int64(position),
	)
	if err != nil {
		return fmt.Errorf("sqlite: delete dead letter of %q segment %d at %d: %w", l.Name, l.Segment, position, err)
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
