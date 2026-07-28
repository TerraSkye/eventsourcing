package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	cqrs "github.com/terraskye/eventsourcing"
)

const pgUniqueViolation = "23505"

var _ cqrs.EventStore = (*eventstore)(nil)

// eventstore is a PostgreSQL-backed [cqrs.EventStore]. Stream membership and
// per-stream ordering are recorded in the events table's stream_id and
// stream_position columns; the table's own auto-incrementing id column
// provides the global order consumed by LoadFromAll.
type eventstore struct {
	pool *pgxpool.Pool
}

// NewEventStore returns a PostgreSQL-backed [cqrs.EventStore]. The pool must
// already be configured to connect to a database containing the events
// table; NewEventStore does not create or migrate the schema.
func NewEventStore(pool *pgxpool.Pool) cqrs.EventStore {
	return &eventstore{pool: pool}
}

// Save appends events to the stream they share, enforcing the concurrency
// check described by revision: [cqrs.Any] skips it, [cqrs.NoStream] requires
// the stream not to exist yet, [cqrs.StreamExists] requires that it already
// does, and a [cqrs.Revision] requires the stream's current length to match
// exactly. All events must have the same StreamID, or Save returns a
// non-nil error without appending anything. The whole append happens inside
// a transaction serialized against other writers to the same stream by a
// Postgres advisory lock, so on a revision mismatch — whether caught by the
// check above or by a unique-constraint violation from a concurrent writer —
// it returns a [cqrs.StreamRevisionConflictError] and nothing is persisted.
// On success it returns a [cqrs.AppendResult] whose NextExpectedVersion is
// the stream's new length.
func (s *eventstore) Save(ctx context.Context, events []cqrs.Envelope, revision cqrs.StreamState) (cqrs.AppendResult, error) {
	if len(events) == 0 {
		return cqrs.AppendResult{Successful: true}, nil
	}

	streamID := events[0].StreamID
	for i, e := range events {
		if e.StreamID != streamID {
			return cqrs.AppendResult{StreamID: streamID}, fmt.Errorf(
				"save events to stream %q: %w: event %d has different stream ID %q",
				streamID, cqrs.ErrInvalidEventBatch, i, e.StreamID,
			)
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return cqrs.AppendResult{StreamID: streamID}, fmt.Errorf("save events to stream %q: begin tx: %w", streamID, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Acquire an advisory lock on the stream to serialize concurrent writes.
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", streamID); err != nil {
		return cqrs.AppendResult{StreamID: streamID}, fmt.Errorf("save events to stream %q: acquire lock: %w", streamID, err)
	}

	var currentVersion uint64
	var maxPos pgtype.Int8
	if err = tx.QueryRow(ctx, "SELECT MAX(stream_position) FROM events WHERE stream_id = $1", streamID).Scan(&maxPos); err != nil {
		return cqrs.AppendResult{StreamID: streamID}, fmt.Errorf("save events to stream %q: get stream version: %w", streamID, err)
	}
	if maxPos.Valid {
		currentVersion = uint64(maxPos.Int64)
	}

	switch rev := revision.(type) {
	case cqrs.Any:
		// no check
	case cqrs.NoStream:
		if currentVersion != 0 {
			return cqrs.AppendResult{Successful: false, StreamID: streamID},
				fmt.Errorf("stream %q: already exists: %w", streamID, cqrs.ErrStreamExists)
		}
	case cqrs.StreamExists:
		if currentVersion == 0 {
			return cqrs.AppendResult{Successful: false, StreamID: streamID},
				fmt.Errorf("stream %q: does not exist: %w", streamID, cqrs.ErrStreamNotFound)
		}
	case cqrs.Revision:
		expected := uint64(rev)
		if currentVersion != expected {
			return cqrs.AppendResult{Successful: false, StreamID: streamID},
				&cqrs.StreamRevisionConflictError{
					Stream:           streamID,
					ExpectedRevision: rev,
					ActualRevision:   cqrs.Revision(currentVersion),
				}
		}
	default:
		return cqrs.AppendResult{Successful: false, StreamID: streamID},
			fmt.Errorf("unsupported revision type for stream %s: %w", streamID, cqrs.ErrInvalidRevision)
	}

	nextPosition := currentVersion + 1

	for i, e := range events {
		payload, err := json.Marshal(e.Event)
		if err != nil {
			return cqrs.AppendResult{Successful: false, StreamID: streamID},
				fmt.Errorf("save events to stream %q: marshal event %d: %w", streamID, i, err)
		}

		meta := e.Metadata
		if meta == nil {
			meta = map[string]any{}
		}
		metadataJSON, err := json.Marshal(meta)
		if err != nil {
			return cqrs.AppendResult{Successful: false, StreamID: streamID},
				fmt.Errorf("save events to stream %q: marshal metadata %d: %w", streamID, i, err)
		}

		occurredAt := e.OccurredAt
		if occurredAt.IsZero() {
			occurredAt = time.Now()
		}

		pos := int64(nextPosition) + int64(i)
		eventID := e.EventID
		if eventID == uuid.Nil {
			eventID = uuid.New()
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO events (event_id, stream_id, stream_position, event_type, payload, metadata, occurred_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			eventID, streamID, pos, e.Event.EventType(), payload, metadataJSON, occurredAt,
		)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
				return cqrs.AppendResult{Successful: false, StreamID: streamID},
					&cqrs.StreamRevisionConflictError{
						Stream:           streamID,
						ExpectedRevision: revision,
						ActualRevision:   cqrs.Revision(currentVersion),
					}
			}
			return cqrs.AppendResult{Successful: false, StreamID: streamID},
				fmt.Errorf("save events to stream %q: insert event %d: %w", streamID, i, err)
		}
	}

	if err = tx.Commit(ctx); err != nil {
		return cqrs.AppendResult{Successful: false, StreamID: streamID},
			fmt.Errorf("save events to stream %q: commit: %w", streamID, err)
	}

	finalVersion := currentVersion + uint64(len(events))
	return cqrs.AppendResult{
		Successful:          true,
		StreamID:            streamID,
		NextExpectedVersion: finalVersion,
	}, nil
}

// LoadStream returns a lazy iterator over all events in the stream
// identified by id, in the order they were appended. It returns a non-nil
// error if the stream does not exist.
func (s *eventstore) LoadStream(ctx context.Context, id string) (*cqrs.Iterator[*cqrs.Envelope], error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM events WHERE stream_id = $1)", id).Scan(&exists); err != nil {
		return nil, fmt.Errorf("load stream %q: check existence: %w", id, err)
	}
	if !exists {
		return nil, fmt.Errorf("load stream %q: %w", id, cqrs.ErrStreamNotFound)
	}

	return s.queryRows(ctx, `
		SELECT id, event_id, stream_id, stream_position, event_type, payload, metadata, occurred_at
		FROM events WHERE stream_id = $1 ORDER BY stream_position ASC`, id)
}

// LoadStreamFrom returns a lazy iterator over the events in the stream
// identified by id, starting after the position identified by version.
// [cqrs.NoStream] requires the stream not to exist and returns a non-nil
// error if it does; [cqrs.StreamExists] requires that it does exist and
// returns a non-nil error if it does not; [cqrs.Any] reads from the
// beginning of the stream. Any other [cqrs.StreamState] — in practice a
// [cqrs.Revision] of n — is treated as a position, and events after it are
// returned, so a caller that has already processed n events resumes exactly
// where it left off; a zero revision behaves like reading from the
// beginning.
func (s *eventstore) LoadStreamFrom(ctx context.Context, id string, version cqrs.StreamState) (*cqrs.Iterator[*cqrs.Envelope], error) {
	switch version.(type) {
	case cqrs.NoStream:
		var exists bool
		if err := s.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM events WHERE stream_id = $1)", id).Scan(&exists); err != nil {
			return nil, fmt.Errorf("load stream %q: check existence: %w", id, err)
		}
		if exists {
			return nil, fmt.Errorf("load stream %q: expected empty stream: %w", id, cqrs.ErrStreamExists)
		}
		return cqrs.NewSliceIterator([]*cqrs.Envelope{}), nil

	case cqrs.StreamExists:
		var exists bool
		if err := s.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM events WHERE stream_id = $1)", id).Scan(&exists); err != nil {
			return nil, fmt.Errorf("load stream %q: check existence: %w", id, err)
		}
		if !exists {
			return nil, fmt.Errorf("load stream %q: expected existing stream: %w", id, cqrs.ErrStreamNotFound)
		}
		return s.queryRows(ctx, `
			SELECT id, event_id, stream_id, stream_position, event_type, payload, metadata, occurred_at
			FROM events WHERE stream_id = $1 ORDER BY stream_position ASC`, id)

	case cqrs.Any:
		return s.queryRows(ctx, `
			SELECT id, event_id, stream_id, stream_position, event_type, payload, metadata, occurred_at
			FROM events WHERE stream_id = $1 ORDER BY stream_position ASC`, id)

	default:
		// Revision: start after the given position
		fromPos := version.ToRawInt64()
		return s.queryRows(ctx, `
			SELECT id, event_id, stream_id, stream_position, event_type, payload, metadata, occurred_at
			FROM events WHERE stream_id = $1 AND stream_position > $2 ORDER BY stream_position ASC`,
			id, fromPos)
	}
}

// LoadFromAll returns a lazy iterator over every event saved across all
// streams, in the global order they were committed, starting after the
// position identified by version (per its ToRawInt64, so [cqrs.Any],
// [cqrs.NoStream], and [cqrs.StreamExists] all behave like reading from the
// very beginning). It only returns events from transactions that had
// already committed when the read began, so a transaction that is still
// in-flight — even if it inserted a row with a lower id than one already
// visible — cannot be skipped over and later missed by a subsequent call
// with a higher version.
func (s *eventstore) LoadFromAll(ctx context.Context, version cqrs.StreamState) (*cqrs.Iterator[*cqrs.Envelope], error) {
	fromID := version.ToRawInt64()
	return s.queryRows(ctx, `
		SELECT id, event_id, stream_id, stream_position, event_type, payload, metadata, occurred_at
		FROM events
		WHERE id > $1
		  AND xmin::text::bigint < pg_snapshot_xmin(pg_current_snapshot())::text::bigint
		ORDER BY id ASC`, fromID)
}

// Close closes the underlying connection pool. It is safe to call more than
// once.
func (s *eventstore) Close() error {
	s.pool.Close()
	return nil
}

// queryRows executes a SELECT and returns a lazy [cqrs.Iterator] over the
// resulting [cqrs.Envelope] values. The underlying rows are held open until
// the iterator is exhausted or an error occurs.
func (s *eventstore) queryRows(ctx context.Context, sql string, args ...any) (*cqrs.Iterator[*cqrs.Envelope], error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}

	return cqrs.NewIteratorFunc(func(ctx context.Context) (*cqrs.Envelope, error) {
		if !rows.Next() {
			rows.Close()
			if err := rows.Err(); err != nil {
				return nil, err
			}
			return nil, io.EOF
		}
		env, err := scanEnvelope(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		return env, nil
	}), nil
}

// scanEnvelope decodes a single events row into a [cqrs.Envelope],
// reconstructing the concrete [cqrs.Event] via [cqrs.NewEventByName].
func scanEnvelope(rows pgx.Rows) (*cqrs.Envelope, error) {
	var (
		globalID       int64
		rawUUID        pgtype.UUID
		streamID       string
		streamPosition int64
		eventType      string
		payload        []byte
		metadata       []byte
		occurredAt     time.Time
	)

	if err := rows.Scan(&globalID, &rawUUID, &streamID, &streamPosition, &eventType, &payload, &metadata, &occurredAt); err != nil {
		return nil, fmt.Errorf("scan envelope: %w", err)
	}

	ev, err := cqrs.NewEventByName(eventType)
	if err != nil {
		return nil, fmt.Errorf("cannot create event %q: %w", eventType, err)
	}

	if err := json.Unmarshal(payload, ev); err != nil {
		return nil, fmt.Errorf("cannot unmarshal event %q: %w", eventType, err)
	}

	var meta map[string]any
	if len(metadata) > 0 {
		if err := json.Unmarshal(metadata, &meta); err != nil {
			meta = make(map[string]any)
		}
	} else {
		meta = make(map[string]any)
	}

	return &cqrs.Envelope{
		EventID:       uuid.UUID(rawUUID.Bytes),
		StreamID:      streamID,
		Event:         ev,
		Metadata:      meta,
		Version:       uint64(streamPosition),
		GlobalVersion: uint64(globalID),
		OccurredAt:    occurredAt,
	}, nil
}
