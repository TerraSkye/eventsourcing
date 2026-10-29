package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/terraskye/eventsourcing"
)

const pgUniqueViolation = "23505"

var (
	_ eventsourcing.EventStore        = (*EventStore)(nil)
	_ eventsourcing.GlobalReader      = (*EventStore)(nil)
	_ eventsourcing.CorrelationReader = (*EventStore)(nil)
)

// appendLockKey is the two-key advisory lock every Save takes before it
// inserts, held until its transaction ends; see Save. The two-key form lives
// in a different lock space from the one-key per-stream locks, so it never
// collides with a stream's hashtext.
const appendLockKey1, appendLockKey2 = 0x65766e74, 0x616c6c00 // "evnt", "all"

// EventStore is a PostgreSQL-backed [eventsourcing.EventStore] that also implements
// [eventsourcing.GlobalReader]. Stream membership and per-stream ordering are recorded
// in the events table's stream_id and stream_position columns; the table's
// own auto-incrementing id column is the GlobalPosition that
// [EventStore.ReadAll] orders by. Create one with [NewEventStore].
type EventStore struct {
	pool *pgxpool.Pool
}

// NewEventStore returns a PostgreSQL-backed [EventStore]. The pool must
// already be configured to connect to a database containing the events
// table; NewEventStore does not create or migrate the schema.
func NewEventStore(pool *pgxpool.Pool) *EventStore {
	return &EventStore{pool: pool}
}

// Save appends events to the stream they share, enforcing the concurrency
// check described by revision: [eventsourcing.Any] skips it, [eventsourcing.NoStream] requires
// the stream not to exist yet, [eventsourcing.StreamExists] requires that it already
// does, and a [eventsourcing.Revision] requires the stream's current length to match
// exactly. All events must have the same StreamID, or Save returns a
// non-nil error without appending anything. The whole append happens inside
// a transaction serialized against other writers to the same stream by a
// Postgres advisory lock, so on a revision mismatch — whether caught by the
// check above or by a unique-constraint violation from a concurrent writer —
// it returns a [eventsourcing.StreamRevisionConflictError] and nothing is persisted.
//
// Before inserting, Save also takes one advisory lock shared by every
// append, held until it commits. Ids are assigned at INSERT but become
// visible at COMMIT; without that lock, a transaction could commit id 52
// while another still holds 51, and a reader that moved past 52 would
// never see 51. With it, appends assign ids and commit one at a time, so
// ids become visible in order and [EventStore.ReadAll] needs no visibility
// filter. The cost is that appends to different streams no longer commit in
// parallel; only the inserts and the commit run under the lock.
//
// On success it returns a [eventsourcing.AppendResult] whose NextExpectedVersion is
// the stream's new length.
func (s *EventStore) Save(ctx context.Context, events []eventsourcing.Envelope, revision eventsourcing.StreamState) (eventsourcing.AppendResult, error) {
	if len(events) == 0 {
		return eventsourcing.AppendResult{Successful: true}, nil
	}

	streamID := events[0].StreamID
	for i, e := range events {
		if e.StreamID != streamID {
			return eventsourcing.AppendResult{StreamID: streamID}, fmt.Errorf(
				"save events to stream %q: %w: event %d has different stream ID %q",
				streamID, eventsourcing.ErrInvalidEventBatch, i, e.StreamID,
			)
		}
	}

	// Encode before taking any lock, so the locks cover only the database
	// work.
	type row struct {
		eventID       uuid.UUID
		eventType     string
		payload       []byte
		metadata      []byte
		occurredAt    time.Time
		correlationID string
		causationID   string
	}
	rows := make([]row, len(events))
	for i, e := range events {
		payload, err := eventsourcing.EncodeEvent(e.Event)
		if err != nil {
			return eventsourcing.AppendResult{Successful: false, StreamID: streamID},
				fmt.Errorf("save events to stream %q: event %d: %w", streamID, i, err)
		}

		metadataJSON, err := eventsourcing.EncodeMetadata(e.Metadata)
		if err != nil {
			return eventsourcing.AppendResult{Successful: false, StreamID: streamID},
				fmt.Errorf("save events to stream %q: event %d: %w", streamID, i, err)
		}

		occurredAt := e.OccurredAt
		if occurredAt.IsZero() {
			occurredAt = time.Now()
		}

		eventID := e.EventID
		if eventID == uuid.Nil {
			eventID = uuid.New()
		}

		rows[i] = row{eventID, e.Event.EventType(), payload, metadataJSON, occurredAt, e.CorrelationID, e.CausationID}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return eventsourcing.AppendResult{StreamID: streamID}, fmt.Errorf("save events to stream %q: begin tx: %w", streamID, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Acquire an advisory lock on the stream to serialize concurrent writes.
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", streamID); err != nil {
		return eventsourcing.AppendResult{StreamID: streamID}, fmt.Errorf("save events to stream %q: acquire lock: %w", streamID, err)
	}

	var currentVersion uint64
	var maxPos pgtype.Int8
	if err = tx.QueryRow(ctx, "SELECT MAX(stream_position) FROM events WHERE stream_id = $1", streamID).Scan(&maxPos); err != nil {
		return eventsourcing.AppendResult{StreamID: streamID}, fmt.Errorf("save events to stream %q: get stream version: %w", streamID, err)
	}
	if maxPos.Valid {
		currentVersion = uint64(maxPos.Int64)
	}

	switch rev := revision.(type) {
	case eventsourcing.Any:
		// no check
	case eventsourcing.NoStream:
		if currentVersion != 0 {
			return eventsourcing.AppendResult{Successful: false, StreamID: streamID},
				fmt.Errorf("stream %q: already exists: %w", streamID, eventsourcing.ErrStreamExists)
		}
	case eventsourcing.StreamExists:
		if currentVersion == 0 {
			return eventsourcing.AppendResult{Successful: false, StreamID: streamID},
				fmt.Errorf("stream %q: does not exist: %w", streamID, eventsourcing.ErrStreamNotFound)
		}
	case eventsourcing.Revision:
		expected := uint64(rev)
		if currentVersion != expected {
			return eventsourcing.AppendResult{Successful: false, StreamID: streamID},
				&eventsourcing.StreamRevisionConflictError{
					Stream:           streamID,
					ExpectedRevision: rev,
					ActualRevision:   eventsourcing.Revision(currentVersion),
				}
		}
	default:
		return eventsourcing.AppendResult{Successful: false, StreamID: streamID},
			fmt.Errorf("unsupported revision type for stream %s: %w", streamID, eventsourcing.ErrInvalidRevision)
	}

	// Serialize every append from here to COMMIT; see the doc comment. It is
	// taken after the per-stream lock and the revision check, always in that
	// order, so two Saves cannot deadlock on the pair.
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1, $2)", appendLockKey1, appendLockKey2); err != nil {
		return eventsourcing.AppendResult{StreamID: streamID}, fmt.Errorf("save events to stream %q: acquire append lock: %w", streamID, err)
	}

	nextPosition := currentVersion + 1

	var lastID int64 // id of the last event inserted: its GlobalPosition
	for i, r := range rows {
		pos := int64(nextPosition) + int64(i)
		err = tx.QueryRow(ctx, `
			INSERT INTO events (event_id, stream_id, stream_position, event_type, payload, metadata, occurred_at, correlation_id, causation_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			RETURNING id`,
			r.eventID, streamID, pos, r.eventType, r.payload, r.metadata, r.occurredAt, r.correlationID, r.causationID,
		).Scan(&lastID)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
				return eventsourcing.AppendResult{Successful: false, StreamID: streamID},
					&eventsourcing.StreamRevisionConflictError{
						Stream:           streamID,
						ExpectedRevision: revision,
						ActualRevision:   eventsourcing.Revision(currentVersion),
					}
			}
			return eventsourcing.AppendResult{Successful: false, StreamID: streamID},
				fmt.Errorf("save events to stream %q: insert event %d: %w", streamID, i, err)
		}
	}

	if err = tx.Commit(ctx); err != nil {
		return eventsourcing.AppendResult{Successful: false, StreamID: streamID},
			fmt.Errorf("save events to stream %q: commit: %w", streamID, err)
	}

	finalVersion := currentVersion + uint64(len(events))
	return eventsourcing.AppendResult{
		Successful:          true,
		StreamID:            streamID,
		NextExpectedVersion: finalVersion,
		GlobalPosition:      uint64(lastID),
	}, nil
}

// LoadStream returns a lazy iterator over all events in the stream
// identified by id, in the order they were appended. It returns a non-nil
// error if the stream does not exist.
func (s *EventStore) LoadStream(ctx context.Context, id string) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM events WHERE stream_id = $1)", id).Scan(&exists); err != nil {
		return nil, fmt.Errorf("load stream %q: check existence: %w", id, err)
	}
	if !exists {
		return nil, fmt.Errorf("load stream %q: %w", id, eventsourcing.ErrStreamNotFound)
	}

	return s.queryRows(ctx, `
		SELECT id, event_id, stream_id, stream_position, event_type, payload, metadata, occurred_at, correlation_id, causation_id
		FROM events WHERE stream_id = $1 ORDER BY stream_position ASC`, id)
}

// LoadStreamFrom returns a lazy iterator over the events in the stream
// identified by id, starting after the position identified by version.
// [eventsourcing.NoStream] requires the stream not to exist and returns a non-nil
// error if it does; [eventsourcing.StreamExists] requires that it does exist and
// returns a non-nil error if it does not; [eventsourcing.Any] reads from the
// beginning of the stream. Any other [eventsourcing.StreamState] — in practice a
// [eventsourcing.Revision] of n — is treated as a position, and events after it are
// returned, so a caller that has already processed n events resumes exactly
// where it left off; a zero revision behaves like reading from the
// beginning.
func (s *EventStore) LoadStreamFrom(ctx context.Context, id string, version eventsourcing.StreamState) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	switch version.(type) {
	case eventsourcing.NoStream:
		var exists bool
		if err := s.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM events WHERE stream_id = $1)", id).Scan(&exists); err != nil {
			return nil, fmt.Errorf("load stream %q: check existence: %w", id, err)
		}
		if exists {
			return nil, fmt.Errorf("load stream %q: expected empty stream: %w", id, eventsourcing.ErrStreamExists)
		}
		return eventsourcing.NewSliceIterator(ctx, []*eventsourcing.Envelope{}), nil

	case eventsourcing.StreamExists:
		var exists bool
		if err := s.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM events WHERE stream_id = $1)", id).Scan(&exists); err != nil {
			return nil, fmt.Errorf("load stream %q: check existence: %w", id, err)
		}
		if !exists {
			return nil, fmt.Errorf("load stream %q: expected existing stream: %w", id, eventsourcing.ErrStreamNotFound)
		}
		return s.queryRows(ctx, `
			SELECT id, event_id, stream_id, stream_position, event_type, payload, metadata, occurred_at, correlation_id, causation_id
			FROM events WHERE stream_id = $1 ORDER BY stream_position ASC`, id)

	case eventsourcing.Any:
		return s.queryRows(ctx, `
			SELECT id, event_id, stream_id, stream_position, event_type, payload, metadata, occurred_at, correlation_id, causation_id
			FROM events WHERE stream_id = $1 ORDER BY stream_position ASC`, id)

	default:
		// Revision: start after the given position
		fromPos := version.ToRawInt64()
		return s.queryRows(ctx, `
			SELECT id, event_id, stream_id, stream_position, event_type, payload, metadata, occurred_at, correlation_id, causation_id
			FROM events WHERE stream_id = $1 AND stream_position > $2 ORDER BY stream_position ASC`,
			id, fromPos)
	}
}

// LoadCorrelation implements [eventsourcing.CorrelationReader], using the
// index on correlation_id. Like [EventStore.LoadStream], the iterator holds
// a pooled connection until it ends or is closed.
func (s *EventStore) LoadCorrelation(ctx context.Context, id string) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	if id == "" {
		return nil, fmt.Errorf("load correlation: %w", eventsourcing.ErrEmptyCorrelationID)
	}
	it, err := s.queryRows(ctx, `
		SELECT id, event_id, stream_id, stream_position, event_type, payload, metadata, occurred_at, correlation_id, causation_id
		FROM events WHERE correlation_id = $1 ORDER BY id ASC`, id)
	if err != nil {
		return nil, fmt.Errorf("load correlation %q: %w", id, err)
	}
	return it, nil
}

// ReadAll implements [eventsourcing.GlobalReader]. Filters run in SQL, on exact
// event type names and on stream ID prefixes (with starts_with, so % and _
// in a prefix are not wildcards).
//
// Because Save commits appends one at a time in id order, every row up to
// the highest visible id is either committed or permanently abandoned (a
// rolled-back insert leaves a hole in the sequence, never a late row), so
// ReadAll reads up to that id without a visibility filter.
func (s *EventStore) ReadAll(ctx context.Context, after uint64, opts eventsourcing.ReadOptions) (eventsourcing.ReadResult, error) {
	if after > math.MaxInt64 {
		return eventsourcing.ReadResult{}, fmt.Errorf("%w: read all after %d", eventsourcing.ErrInvalidRevision, after)
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 500
	}

	// Pin the head first and read no further than it, so Next and AtEnd
	// describe the same point in the log even if appends land in between.
	head, err := s.Head(ctx)
	if err != nil {
		return eventsourcing.ReadResult{}, fmt.Errorf("read all: %w", err)
	}
	res := eventsourcing.ReadResult{Next: max(after, head), AtEnd: true}
	if head <= after {
		res.Next = after
		return res, nil
	}

	types := opts.EventTypes
	if types == nil {
		types = []string{}
	}
	prefixes := opts.StreamPrefixes
	if prefixes == nil {
		prefixes = []string{}
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, event_id, stream_id, stream_position, event_type, payload, metadata, occurred_at, correlation_id, causation_id
		FROM events
		WHERE id > $1 AND id <= $2
		  AND (cardinality($3::text[]) = 0 OR event_type = ANY($3::text[]))
		  AND (cardinality($4::text[]) = 0 OR EXISTS (
		        SELECT 1 FROM unnest($4::text[]) AS p WHERE starts_with(stream_id, p)))
		ORDER BY id ASC
		LIMIT $5`, int64(after), int64(head), types, prefixes, limit)
	if err != nil {
		return eventsourcing.ReadResult{}, fmt.Errorf("read all: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		env, err := scanEnvelope(rows)
		if err != nil {
			return eventsourcing.ReadResult{}, fmt.Errorf("read all: %w", err)
		}
		res.Events = append(res.Events, env)
	}
	if err := rows.Err(); err != nil {
		return eventsourcing.ReadResult{}, fmt.Errorf("read all: %w", err)
	}

	// A full page may stop short of the head; anything less means every
	// row up to the head was scanned.
	if len(res.Events) == limit {
		res.Next = res.Events[len(res.Events)-1].GlobalPosition
		res.AtEnd = res.Next >= head
	}
	return res, nil
}

// Head implements [eventsourcing.GlobalReader]: the highest id in the events table,
// or 0 if it is empty.
func (s *EventStore) Head(ctx context.Context) (uint64, error) {
	var head int64
	if err := s.pool.QueryRow(ctx, "SELECT coalesce(max(id), 0) FROM events").Scan(&head); err != nil {
		return 0, fmt.Errorf("head: %w", err)
	}
	return uint64(head), nil
}

// Close closes the underlying connection pool. It is safe to call more than
// once.
func (s *EventStore) Close() error {
	s.pool.Close()
	return nil
}

// queryRows executes a SELECT and returns a lazy [eventsourcing.Iterator] over the
// resulting [eventsourcing.Envelope] values. The underlying rows are held open until
// the iterator is exhausted or an error occurs.
func (s *EventStore) queryRows(ctx context.Context, sql string, args ...any) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}

	return eventsourcing.NewIteratorFunc(ctx, func(ctx context.Context) (*eventsourcing.Envelope, error) {
		if !rows.Next() {
			if err := rows.Err(); err != nil {
				return nil, err
			}
			return nil, io.EOF
		}
		env, err := scanEnvelope(rows)
		if err != nil {

			return nil, err
		}
		return env, nil
	}, func() error {
		rows.Close()
		return nil
	}), nil
}

// scanEnvelope decodes a single events row into a [eventsourcing.Envelope],
// reconstructing the concrete [eventsourcing.Event] via [eventsourcing.DecodeEvent].
func scanEnvelope(rows pgx.Rows) (*eventsourcing.Envelope, error) {
	var (
		globalID       int64
		rawUUID        pgtype.UUID
		streamID       string
		streamPosition int64
		eventType      string
		payload        []byte
		metadata       []byte
		occurredAt     time.Time
		correlationID  string
		causationID    string
	)

	if err := rows.Scan(&globalID, &rawUUID, &streamID, &streamPosition, &eventType, &payload, &metadata, &occurredAt, &correlationID, &causationID); err != nil {
		return nil, fmt.Errorf("scan envelope: %w", err)
	}

	ev, err := eventsourcing.DecodeEvent(eventType, payload)
	if err != nil {
		return nil, fmt.Errorf("scan envelope: %w", err)
	}

	meta, err := eventsourcing.DecodeMetadata(metadata)
	if err != nil {
		meta = make(map[string]any)
	}

	return &eventsourcing.Envelope{
		EventID:        uuid.UUID(rawUUID.Bytes),
		StreamID:       streamID,
		Event:          ev,
		Metadata:       meta,
		Version:        uint64(streamPosition),
		GlobalPosition: uint64(globalID),
		OccurredAt:     occurredAt,
		CorrelationID:  correlationID,
		CausationID:    causationID,
	}, nil
}
