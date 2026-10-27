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
// owner and epoch are the row's values before the update, and WHERE judges
// expiry by the current claim's own ttl_ms.
func (s *ProjectionStore) Claim(ctx context.Context, l projection.Lease, ttl time.Duration) (projection.Lease, projection.Checkpoint, bool, error) {
	var (
		cp          projection.Checkpoint
		epoch       int64
		position    int64
		replayUntil int64
	)
	err := s.db.QueryRowContext(ctx, `
		UPDATE projection_segments
		SET owner = ?3, heartbeat = `+nowMS+`, ttl_ms = ?4,
		    epoch = CASE WHEN owner = ?3 AND epoch = ?5 THEN epoch ELSE epoch + 1 END
		WHERE name = ?1 AND segment = ?2
		  AND ((owner = ?3 AND epoch = ?5)
		       OR owner IS NULL
		       OR heartbeat < `+nowMS+` - ttl_ms)
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
		// The segments' rows go first, so this does not rely on foreign
		// keys being on for the connection.
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM projection_replay_segments
			WHERE replay IN (SELECT id FROM projection_replays WHERE name = ?)`, l.Name); err != nil {
			return fmt.Errorf("sqlite: clear replay requests of %q: %w", l.Name, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM projection_replays WHERE name = ?`, l.Name); err != nil {
			return fmt.Errorf("sqlite: clear replay requests of %q: %w", l.Name, err)
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
		       `+nowMS+` - s.heartbeat, s.ttl_ms,
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
			ttlMS    int64
			replay   int64
			pending  bool
		)
		if err := rows.Scan(&st.Segment, &phase, &position, &st.Version, &head, &lastErr, &owner, &ageMS, &ttlMS, &replay, &pending); err != nil {
			return nil, fmt.Errorf("sqlite: load statuses of %q: %w", name, err)
		}
		st.Phase = parsePhase(phase)
		st.Position = uint64(position)
		st.Head = uint64(head)
		st.LastError = lastErr.String
		st.Owner = owner.String
		if owner.Valid {
			st.ClaimTTL = time.Duration(ttlMS) * time.Millisecond
		}
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
// owner's row, deletes the rows whose own ttl has passed, and reads the
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
			INSERT INTO projection_instances (name, owner, version, heartbeat, ttl_ms)
			VALUES (?1, ?2, ?3, `+nowMS+`, ?4)
			ON CONFLICT (name, owner) DO UPDATE
			SET version = excluded.version, heartbeat = excluded.heartbeat, ttl_ms = excluded.ttl_ms`,
			name, owner, version, ttl.Milliseconds(),
		); err != nil {
			return fmt.Errorf("sqlite: heartbeat %q as %q: %w", name, owner, err)
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM projection_instances WHERE name = ? AND heartbeat < `+nowMS+` - ttl_ms`,
			name,
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

// LoadInstances implements [projection.Store]: it reads the rows whose own
// ttl has not passed, with the number of segments each one claims.
func (s *ProjectionStore) LoadInstances(ctx context.Context, name string) ([]projection.Instance, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.owner, m.version, `+nowMS+` - m.heartbeat,
		       (SELECT count(*) FROM projection_segments s WHERE s.name = m.name AND s.owner = m.owner)
		FROM projection_instances m
		WHERE m.name = ? AND m.heartbeat >= `+nowMS+` - m.ttl_ms
		ORDER BY m.owner`,
		name,
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: load instances of %q: %w", name, err)
	}
	defer rows.Close()
	var out []projection.Instance
	for rows.Next() {
		var (
			m     projection.Instance
			ageMS int64
		)
		if err := rows.Scan(&m.Owner, &m.Version, &ageMS, &m.Segments); err != nil {
			return nil, fmt.Errorf("sqlite: load instances of %q: %w", name, err)
		}
		m.Age = time.Duration(max(ageMS, 0)) * time.Millisecond
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: load instances of %q: %w", name, err)
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

// FailRebuild implements [projection.Store], fenced on l in the same
// transaction.
func (s *ProjectionStore) FailRebuild(ctx context.Context, l projection.Lease, requested uint64, errMsg string) error {
	return s.runTx(ctx, "fail rebuild", l.Name, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := ownedPosition(ctx, tx, l); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE projection_rebuilds SET failed = ?2, error = ?3 WHERE name = ?1 AND failed <= ?2`,
			l.Name, int64(requested), errMsg,
		); err != nil {
			return fmt.Errorf("sqlite: fail rebuild %d of %q: %w", requested, l.Name, err)
		}
		return nil
	})
}

// RebuildFailure implements [projection.Store].
func (s *ProjectionStore) RebuildFailure(ctx context.Context, name string) (failed uint64, errMsg string, err error) {
	var (
		f   int64
		msg sql.NullString
	)
	err = s.db.QueryRowContext(ctx,
		`SELECT failed, error FROM projection_rebuilds WHERE name = ?`, name,
	).Scan(&f, &msg)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", nil
	}
	if err != nil {
		return 0, "", fmt.Errorf("sqlite: read rebuild failure of %q: %w", name, err)
	}
	return uint64(f), msg.String, nil
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

// RequestReplay implements [projection.DeadLetterStore], in one transaction
// that records the request and a row for each segment holding dead letters
// of streamID. A stream without dead letters records nothing and gets 0.
func (s *ProjectionStore) RequestReplay(ctx context.Context, name, streamID string) (uint64, error) {
	var n int64
	err := s.runTx(ctx, "request replay", name, func(ctx context.Context, tx *sql.Tx) error {
		var declared bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM projection_segments WHERE name = ?)`, name,
		).Scan(&declared); err != nil {
			return fmt.Errorf("sqlite: request replay of %q stream %q: %w", name, streamID, err)
		}
		if !declared {
			return projection.ErrUnknownProjection
		}
		var held bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM projection_dead_letters WHERE projection = ? AND stream_id = ?)`, name, streamID,
		).Scan(&held); err != nil {
			return fmt.Errorf("sqlite: request replay of %q stream %q: %w", name, streamID, err)
		}
		if !held {
			return nil
		}
		if err := tx.QueryRowContext(ctx,
			`INSERT INTO projection_replays (name, stream_id) VALUES (?, ?) RETURNING id`, name, streamID,
		).Scan(&n); err != nil {
			return fmt.Errorf("sqlite: request replay of %q stream %q: %w", name, streamID, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO projection_replay_segments (replay, segment)
			SELECT DISTINCT ?, segment FROM projection_dead_letters WHERE projection = ? AND stream_id = ?`,
			n, name, streamID,
		); err != nil {
			return fmt.Errorf("sqlite: request replay of %q stream %q: %w", name, streamID, err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return uint64(n), nil
}

// PendingReplays implements [projection.DeadLetterStore].
func (s *ProjectionStore) PendingReplays(ctx context.Context, name string) ([]projection.Replay, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.stream_id, s.segment
		FROM projection_replays r JOIN projection_replay_segments s ON s.replay = r.id
		WHERE r.name = ? AND NOT s.done
		ORDER BY r.id, s.segment`,
		name,
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: load replay requests of %q: %w", name, err)
	}
	defer rows.Close()
	var out []projection.Replay
	for rows.Next() {
		var (
			rp projection.Replay
			n  int64
		)
		if err := rows.Scan(&n, &rp.StreamID, &rp.Segment); err != nil {
			return nil, fmt.Errorf("sqlite: load replay requests of %q: %w", name, err)
		}
		rp.Number = uint64(n)
		out = append(out, rp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: load replay requests of %q: %w", name, err)
	}
	return out, nil
}

// FinishReplay implements [projection.DeadLetterStore], fenced on l in the
// same transaction.
func (s *ProjectionStore) FinishReplay(ctx context.Context, l projection.Lease, number uint64, errMsg string) error {
	return s.runTx(ctx, "finish replay", l.Name, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := ownedPosition(ctx, tx, l); err != nil {
			return err
		}
		var streamID string
		err := tx.QueryRowContext(ctx,
			`SELECT stream_id FROM projection_replays WHERE id = ? AND name = ?`, int64(number), l.Name,
		).Scan(&streamID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // deleted by Reset
		}
		if err != nil {
			return fmt.Errorf("sqlite: finish replay %d of %q segment %d: %w", number, l.Name, l.Segment, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE projection_replay_segments SET done = 1, error = ? WHERE replay = ? AND segment = ?`,
			nullString(errMsg), int64(number), l.Segment,
		); err != nil {
			return fmt.Errorf("sqlite: finish replay %d of %q segment %d: %w", number, l.Name, l.Segment, err)
		}
		// Once every segment tried it without an error, delete it and the
		// earlier requests of its stream that are finished. The segment
		// rows go first, as the database may not enforce foreign keys.
		finished := `
			SELECT r.id FROM projection_replays r
			WHERE r.name = ? AND r.stream_id = ? AND r.id <= ?
			  AND NOT EXISTS (SELECT 1 FROM projection_replay_segments s WHERE s.replay = r.id AND NOT s.done)
			  AND NOT EXISTS (SELECT 1 FROM projection_replay_segments s WHERE s.replay = ? AND (NOT s.done OR s.error IS NOT NULL))`
		for _, del := range []string{
			`DELETE FROM projection_replay_segments WHERE replay IN (` + finished + `)`,
			`DELETE FROM projection_replays WHERE id IN (` + finished + `)`,
		} {
			if _, err := tx.ExecContext(ctx, del, l.Name, streamID, int64(number), int64(number)); err != nil {
				return fmt.Errorf("sqlite: finish replay %d of %q segment %d: delete finished requests: %w", number, l.Name, l.Segment, err)
			}
		}
		return nil
	})
}

// ReplayResult implements [projection.DeadLetterStore].
func (s *ProjectionStore) ReplayResult(ctx context.Context, name string, number uint64) (int, []string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.done, s.error
		FROM projection_replays r JOIN projection_replay_segments s ON s.replay = r.id
		WHERE r.name = ? AND r.id = ?
		ORDER BY s.segment`,
		name, int64(number),
	)
	if err != nil {
		return 0, nil, fmt.Errorf("sqlite: read replay %d of %q: %w", number, name, err)
	}
	defer rows.Close()
	pending, errs := 0, []string(nil)
	for rows.Next() {
		var (
			done bool
			msg  sql.NullString
		)
		if err := rows.Scan(&done, &msg); err != nil {
			return 0, nil, fmt.Errorf("sqlite: read replay %d of %q: %w", number, name, err)
		}
		switch {
		case !done:
			pending++
		case msg.Valid:
			errs = append(errs, msg.String)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, nil, fmt.Errorf("sqlite: read replay %d of %q: %w", number, name, err)
	}
	return pending, errs, nil
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
