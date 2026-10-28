package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
	sqlite3 "modernc.org/sqlite/lib"
)

var (
	_ eventsourcing.EventStore   = (*EventStore)(nil)
	_ eventsourcing.GlobalReader = (*EventStore)(nil)
)

// defaultReadLimit is the page size ReadAll uses when ReadOptions.Limit is
// not set.
const defaultReadLimit = 500

// EventStore is a SQLite-backed [eventsourcing.EventStore] that also implements
// [eventsourcing.GlobalReader]. The events table's id column is the GlobalPosition
// that [EventStore.ReadAll] orders by. Create one with [NewEventStore].
//
// SQLite lets one writer at a time commit, and a writer holds its lock from
// the first insert until it commits, so ids become visible in the order
// they were assigned, also with several processes on one file. ReadAll
// needs no visibility filter.
type EventStore struct {
	db *sql.DB
}

// NewEventStore returns an [EventStore] on db, which must hold the tables
// [Schema] creates. Open db with [Open].
func NewEventStore(db *sql.DB) *EventStore {
	return &EventStore{db: db}
}

// Save appends events to the stream they share, enforcing the concurrency
// check described by revision: [eventsourcing.Any] skips it, [eventsourcing.NoStream] requires
// the stream not to exist yet, [eventsourcing.StreamExists] requires that it already
// does, and a [eventsourcing.Revision] requires the stream's current length to match
// exactly. All events must have the same StreamID, or Save returns an error
// without appending anything. The append runs in one transaction; on a
// revision mismatch it returns a [eventsourcing.StreamRevisionConflictError] and
// nothing is persisted.
//
// On success it returns a [eventsourcing.AppendResult] whose NextExpectedVersion is
// the stream's new length and whose GlobalPosition is the id of the last
// event appended.
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

	// Encode before the transaction, so it covers only the database work.
	type row struct {
		eventID       string
		eventType     string
		payload       []byte
		metadata      []byte
		occurredAt    int64
		correlationID string
		causationID   string
	}
	rows := make([]row, len(events))
	for i, e := range events {
		payload, err := eventsourcing.EncodeEvent(e.Event)
		if err != nil {
			return eventsourcing.AppendResult{StreamID: streamID},
				fmt.Errorf("save events to stream %q: event %d: %w", streamID, i, err)
		}
		metadataJSON, err := eventsourcing.EncodeMetadata(e.Metadata)
		if err != nil {
			return eventsourcing.AppendResult{StreamID: streamID},
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
		rows[i] = row{eventID.String(), e.Event.EventType(), payload, metadataJSON, occurredAt.UnixNano(), e.CorrelationID, e.CausationID}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return eventsourcing.AppendResult{StreamID: streamID}, fmt.Errorf("save events to stream %q: begin tx: %w", streamID, err)
	}
	defer tx.Rollback() //nolint:errcheck

	var currentVersion uint64
	if err := tx.QueryRowContext(ctx,
		"SELECT coalesce(max(stream_position), 0) FROM events WHERE stream_id = ?", streamID,
	).Scan(&currentVersion); err != nil {
		return eventsourcing.AppendResult{StreamID: streamID}, fmt.Errorf("save events to stream %q: get stream version: %w", streamID, err)
	}

	switch rev := revision.(type) {
	case eventsourcing.Any:
		// no check
	case eventsourcing.NoStream:
		if currentVersion != 0 {
			return eventsourcing.AppendResult{StreamID: streamID},
				fmt.Errorf("stream %q: already exists: %w", streamID, eventsourcing.ErrStreamExists)
		}
	case eventsourcing.StreamExists:
		if currentVersion == 0 {
			return eventsourcing.AppendResult{StreamID: streamID},
				fmt.Errorf("stream %q: does not exist: %w", streamID, eventsourcing.ErrStreamNotFound)
		}
	case eventsourcing.Revision:
		if currentVersion != uint64(rev) {
			return eventsourcing.AppendResult{StreamID: streamID},
				&eventsourcing.StreamRevisionConflictError{
					Stream:           streamID,
					ExpectedRevision: rev,
					ActualRevision:   eventsourcing.Revision(currentVersion),
				}
		}
	default:
		return eventsourcing.AppendResult{StreamID: streamID},
			fmt.Errorf("unsupported revision type for stream %s: %w", streamID, eventsourcing.ErrInvalidRevision)
	}

	var lastID int64
	for i, r := range rows {
		pos := int64(currentVersion) + int64(i) + 1
		err := tx.QueryRowContext(ctx, `
			INSERT INTO events (event_id, stream_id, stream_position, event_type, payload, metadata, occurred_at, correlation_id, causation_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			RETURNING id`,
			r.eventID, streamID, pos, r.eventType, r.payload, r.metadata, r.occurredAt, r.correlationID, r.causationID,
		).Scan(&lastID)
		if err != nil {
			if isUniqueViolation(err) {
				return eventsourcing.AppendResult{StreamID: streamID},
					&eventsourcing.StreamRevisionConflictError{
						Stream:           streamID,
						ExpectedRevision: revision,
						ActualRevision:   eventsourcing.Revision(currentVersion),
					}
			}
			return eventsourcing.AppendResult{StreamID: streamID},
				fmt.Errorf("save events to stream %q: insert event %d: %w", streamID, i, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return eventsourcing.AppendResult{StreamID: streamID},
			fmt.Errorf("save events to stream %q: commit: %w", streamID, err)
	}
	return eventsourcing.AppendResult{
		Successful:          true,
		StreamID:            streamID,
		NextExpectedVersion: currentVersion + uint64(len(events)),
		GlobalPosition:      uint64(lastID),
	}, nil
}

// LoadStream returns an iterator over all events in the stream identified
// by id, in the order they were appended. It returns an error wrapping
// [eventsourcing.ErrStreamNotFound] if the stream does not exist.
//
// The events are read in full before LoadStream returns, so the one
// connection is free again while the caller iterates.
func (s *EventStore) LoadStream(ctx context.Context, id string) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	envs, err := s.query(ctx, selectEvents+" WHERE stream_id = ? ORDER BY stream_position", id)
	if err != nil {
		return nil, fmt.Errorf("load stream %q: %w", id, err)
	}
	if len(envs) == 0 {
		return nil, fmt.Errorf("load stream %q: %w", id, eventsourcing.ErrStreamNotFound)
	}
	return eventsourcing.NewSliceIterator(ctx, envs), nil
}

// LoadStreamFrom returns an iterator over the events in the stream
// identified by id, starting after the position identified by version.
// [eventsourcing.NoStream] requires the stream not to exist; [eventsourcing.StreamExists]
// requires that it does; [eventsourcing.Any] reads from the beginning of the stream.
// A [eventsourcing.Revision] of n returns the events after the n-th, so a caller
// that has processed n events resumes where it left off.
func (s *EventStore) LoadStreamFrom(ctx context.Context, id string, version eventsourcing.StreamState) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	switch version.(type) {
	case eventsourcing.NoStream:
		exists, err := s.exists(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("load stream %q: %w", id, err)
		}
		if exists {
			return nil, fmt.Errorf("load stream %q: expected empty stream: %w", id, eventsourcing.ErrStreamExists)
		}
		return eventsourcing.NewSliceIterator(ctx, []*eventsourcing.Envelope{}), nil

	case eventsourcing.StreamExists:
		envs, err := s.query(ctx, selectEvents+" WHERE stream_id = ? ORDER BY stream_position", id)
		if err != nil {
			return nil, fmt.Errorf("load stream %q: %w", id, err)
		}
		if len(envs) == 0 {
			return nil, fmt.Errorf("load stream %q: expected existing stream: %w", id, eventsourcing.ErrStreamNotFound)
		}
		return eventsourcing.NewSliceIterator(ctx, envs), nil

	case eventsourcing.Any:
		envs, err := s.query(ctx, selectEvents+" WHERE stream_id = ? ORDER BY stream_position", id)
		if err != nil {
			return nil, fmt.Errorf("load stream %q: %w", id, err)
		}
		return eventsourcing.NewSliceIterator(ctx, envs), nil

	default:
		envs, err := s.query(ctx, selectEvents+" WHERE stream_id = ? AND stream_position > ? ORDER BY stream_position",
			id, version.ToRawInt64())
		if err != nil {
			return nil, fmt.Errorf("load stream %q: %w", id, err)
		}
		return eventsourcing.NewSliceIterator(ctx, envs), nil
	}
}

// ReadAll implements [eventsourcing.GlobalReader]. Filters run in SQL, on exact
// event type names and on stream ID prefixes (compared with substr, so %
// and _ in a prefix are not wildcards).
func (s *EventStore) ReadAll(ctx context.Context, after uint64, opts eventsourcing.ReadOptions) (eventsourcing.ReadResult, error) {
	if after > math.MaxInt64 {
		return eventsourcing.ReadResult{}, fmt.Errorf("%w: read all after %d", eventsourcing.ErrInvalidRevision, after)
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultReadLimit
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

	var (
		where strings.Builder
		args  = []any{int64(after), int64(head)}
	)
	where.WriteString(" WHERE id > ? AND id <= ?")
	if len(opts.EventTypes) > 0 {
		where.WriteString(" AND event_type IN (" + placeholders(len(opts.EventTypes)) + ")")
		for _, t := range opts.EventTypes {
			args = append(args, t)
		}
	}
	if len(opts.StreamPrefixes) > 0 {
		where.WriteString(" AND (")
		for i, p := range opts.StreamPrefixes {
			if i > 0 {
				where.WriteString(" OR ")
			}
			where.WriteString("substr(stream_id, 1, ?) = ?")
			args = append(args, len(p), p)
		}
		where.WriteString(")")
	}
	args = append(args, limit)

	res.Events, err = s.query(ctx, selectEvents+where.String()+" ORDER BY id LIMIT ?", args...)
	if err != nil {
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
	if err := s.db.QueryRowContext(ctx, "SELECT coalesce(max(id), 0) FROM events").Scan(&head); err != nil {
		return 0, fmt.Errorf("head: %w", err)
	}
	return uint64(head), nil
}

// Close does nothing: the *sql.DB belongs to the caller, who may share it
// with an [EventBus] and a [ProjectionStore], and closes it when done.
func (s *EventStore) Close() error {
	return nil
}

func (s *EventStore) exists(ctx context.Context, id string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM events WHERE stream_id = ?)", id).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check existence: %w", err)
	}
	return exists, nil
}

// selectEvents is the column list scanEnvelope reads, without a WHERE
// clause.
const selectEvents = `
	SELECT id, event_id, stream_id, stream_position, event_type, payload, metadata, occurred_at, correlation_id, causation_id
	FROM events`

// query runs a SELECT built on selectEvents and returns every row, read in
// full and closed.
func (s *EventStore) query(ctx context.Context, query string, args ...any) ([]*eventsourcing.Envelope, error) {
	return queryEnvelopes(ctx, s.db, query, args...)
}

// queryer is what queryEnvelopes needs from a *sql.DB or *sql.Tx.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func queryEnvelopes(ctx context.Context, q queryer, query string, args ...any) ([]*eventsourcing.Envelope, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*eventsourcing.Envelope
	for rows.Next() {
		env, err := scanEnvelope(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, env)
	}
	return out, rows.Err()
}

// scanEnvelope decodes a single events row into a [eventsourcing.Envelope],
// reconstructing the concrete [eventsourcing.Event] via [eventsourcing.DecodeEvent].
func scanEnvelope(rows *sql.Rows) (*eventsourcing.Envelope, error) {
	var (
		globalID       int64
		eventID        string
		streamID       string
		streamPosition int64
		eventType      string
		payload        []byte
		metadata       []byte
		occurredAt     int64
		correlationID  string
		causationID    string
	)
	if err := rows.Scan(&globalID, &eventID, &streamID, &streamPosition, &eventType, &payload, &metadata, &occurredAt, &correlationID, &causationID); err != nil {
		return nil, fmt.Errorf("scan envelope: %w", err)
	}

	id, err := uuid.Parse(eventID)
	if err != nil {
		return nil, fmt.Errorf("scan envelope: event id %q: %w", eventID, err)
	}
	ev, err := eventsourcing.DecodeEvent(eventType, payload)
	if err != nil {
		return nil, fmt.Errorf("scan envelope: %w", err)
	}
	meta, err := eventsourcing.DecodeMetadata(metadata)
	if err != nil {
		meta = map[string]any{}
	}

	return &eventsourcing.Envelope{
		EventID:        id,
		StreamID:       streamID,
		Event:          ev,
		Metadata:       meta,
		Version:        uint64(streamPosition),
		GlobalPosition: uint64(globalID),
		OccurredAt:     time.Unix(0, occurredAt),
		CorrelationID:  correlationID,
		CausationID:    causationID,
	}, nil
}

// placeholders returns n comma-separated "?" placeholders.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// isUniqueViolation reports whether err is SQLite's UNIQUE constraint
// failure.
func isUniqueViolation(err error) bool {
	var coder interface{ Code() int }
	return errors.As(err, &coder) && coder.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}
