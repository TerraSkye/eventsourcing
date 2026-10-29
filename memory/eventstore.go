package memory

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/terraskye/eventsourcing"
)

var (
	_ eventsourcing.EventStore        = (*EventStore)(nil)
	_ eventsourcing.GlobalReader      = (*EventStore)(nil)
	_ eventsourcing.CorrelationLoader = (*EventStore)(nil)
)

// EventStore is an in-memory [eventsourcing.EventStore] intended for tests
// and local development. It keeps every stream and the global event log in a
// single process's memory, so its contents do not survive a restart and it
// cannot be shared across processes. It is safe for concurrent use.
//
// Like the persistent stores, it keeps each event encoded by
// [eventsourcing.EncodeEvent] and decodes it on every read, so every event
// type it saves must be registered, and each read returns its own copy of
// the event.
type EventStore struct {
	mu     sync.RWMutex
	closed bool
	global []*record
	events map[string][]*record
}

// record is a saved event as the store keeps it: the envelope with its Event
// and Metadata left nil, beside their encodings and the name the event was
// saved under.
type record struct {
	env      eventsourcing.Envelope
	name     string
	payload  []byte
	metadata []byte
}

// envelope decodes r into a new [eventsourcing.Envelope].
func (r *record) envelope() (*eventsourcing.Envelope, error) {
	ev, err := eventsourcing.DecodeEvent(r.name, r.payload)
	if err != nil {
		return nil, err
	}
	meta, err := eventsourcing.DecodeMetadata(r.metadata)
	if err != nil {
		return nil, fmt.Errorf("event %q: %w", r.name, err)
	}
	env := r.env
	env.Event = ev
	env.Metadata = meta
	return &env, nil
}

// ReadAll implements [eventsourcing.GlobalReader]. Events are kept in a
// single slice in append order and GlobalPosition is their index plus one,
// so a read is a slice of that slice, filtered in Go.
func (m *EventStore) ReadAll(ctx context.Context, after uint64, opts eventsourcing.ReadOptions) (eventsourcing.ReadResult, error) {
	if err := ctx.Err(); err != nil {
		return eventsourcing.ReadResult{}, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 500
	}
	types := make(map[string]bool, len(opts.EventTypes))
	for _, t := range opts.EventTypes {
		types[t] = true
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	res := eventsourcing.ReadResult{Next: after}
	for i := after; i < uint64(len(m.global)); i++ {
		rec := m.global[i]
		res.Next = rec.env.GlobalPosition
		if matches(rec, types, opts.StreamPrefixes) {
			env, err := rec.envelope()
			if err != nil {
				return eventsourcing.ReadResult{}, fmt.Errorf("read all after %d: %w", after, err)
			}
			res.Events = append(res.Events, env)
			if len(res.Events) == limit {
				break
			}
		}
	}
	res.AtEnd = res.Next >= uint64(len(m.global))
	return res, nil
}

func matches(rec *record, types map[string]bool, prefixes []string) bool {
	// An event matches on the name it was saved under, its EventType at the
	// time; aliases do not match, as on the other stores.
	if len(types) > 0 && !types[rec.name] {
		return false
	}
	if len(prefixes) == 0 {
		return true
	}
	for _, p := range prefixes {
		if strings.HasPrefix(rec.env.StreamID, p) {
			return true
		}
	}
	return false
}

// Head implements [eventsourcing.GlobalReader].
func (m *EventStore) Head(ctx context.Context) (uint64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return uint64(len(m.global)), nil
}

// Save appends events to the stream they share, atomically and under the
// store's lock: either all of them are appended or, on a validation or
// concurrency failure, none are. All events must have the same StreamID, or
// Save returns a non-nil error without appending anything.
//
// revision controls the concurrency check: [eventsourcing.Any] skips it,
// [eventsourcing.NoStream] requires the stream not to exist yet,
// [eventsourcing.StreamExists] requires that it already does, and an
// [eventsourcing.Revision] requires the stream's current length to match
// exactly, returning a [eventsourcing.StreamRevisionConflictError] on
// mismatch. On success it returns an [eventsourcing.AppendResult] whose
// NextExpectedVersion is the stream's new length.
func (m *EventStore) Save(ctx context.Context, events []eventsourcing.Envelope, revision eventsourcing.StreamState) (eventsourcing.AppendResult, error) {
	if len(events) == 0 {
		return eventsourcing.AppendResult{Successful: true, NextExpectedVersion: 0}, nil
	}

	streamId := events[0].StreamID
	// Validate all events are for same stream
	for i, env := range events {
		if env.StreamID != streamId {
			return eventsourcing.AppendResult{
					Successful: false,
					StreamID:   streamId,
				}, fmt.Errorf(
					"save events to stream %q: %w: event %d has different stream ID %q",
					streamId, eventsourcing.ErrInvalidEventBatch, i, env.StreamID,
				)
		}
	}

	// Encode before taking the lock, so it covers only the append, and
	// before appending anything, so an event that cannot be encoded fails
	// the whole batch. events belongs to the caller, who may reuse it
	// once Save returns, so each record holds its own copy of the envelope.
	records := make([]*record, len(events))
	for i, env := range events {
		payload, err := eventsourcing.EncodeEvent(env.Event)
		if err != nil {
			return eventsourcing.AppendResult{Successful: false, StreamID: streamId},
				fmt.Errorf("save events to stream %q: event %d: %w", streamId, i, err)
		}
		metadata, err := eventsourcing.EncodeMetadata(env.Metadata)
		if err != nil {
			return eventsourcing.AppendResult{Successful: false, StreamID: streamId},
				fmt.Errorf("save events to stream %q: event %d: %w", streamId, i, err)
		}
		name := env.Event.EventType()
		env.Event = nil
		env.Metadata = nil
		records[i] = &record{env: env, name: name, payload: payload, metadata: metadata}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return eventsourcing.AppendResult{Successful: false}, errors.New("save events: event store is closed")
	}

	currentVersion := uint64(len(m.events[streamId]))

	// Handle revision enforcement
	switch rev := revision.(type) {
	case eventsourcing.Any:
		// No concurrency check
	case eventsourcing.NoStream:
		if currentVersion != 0 {
			err := fmt.Errorf("stream %q: already exists: %w", streamId, eventsourcing.ErrStreamExists)
			return eventsourcing.AppendResult{Successful: false, StreamID: streamId}, err
		}
	case eventsourcing.StreamExists:
		if currentVersion == 0 {
			err := fmt.Errorf("stream %q: should exist: %w ", streamId, eventsourcing.ErrStreamNotFound)
			return eventsourcing.AppendResult{Successful: false, StreamID: streamId}, err
		}
	case eventsourcing.Revision:
		if currentVersion != uint64(rev) {
			return eventsourcing.AppendResult{}, &eventsourcing.StreamRevisionConflictError{
				Stream:           streamId,
				ExpectedRevision: rev,
				ActualRevision:   eventsourcing.Revision(currentVersion),
			}

		}
	default:
		err := fmt.Errorf("unsupported revision type for stream %s :%w", streamId, eventsourcing.ErrInvalidRevision)
		return eventsourcing.AppendResult{Successful: false, StreamID: streamId}, err
	}

	for _, rec := range records {
		rec.env.GlobalPosition = uint64(len(m.global)) + 1
		m.events[streamId] = append(m.events[streamId], rec)
		m.global = append(m.global, rec)
		currentVersion++
	}

	return eventsourcing.AppendResult{
		StreamID:            streamId,
		Successful:          true,
		NextExpectedVersion: currentVersion,
		GlobalPosition:      uint64(len(m.global)),
	}, nil
}

// LoadStream returns a lazy iterator over all events in the stream
// identified by id, in the order they were appended. It returns a non-nil
// error if the stream does not exist.
func (m *EventStore) LoadStream(ctx context.Context, id string) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	return m.LoadStreamFrom(ctx, id, eventsourcing.StreamExists{})
}

// LoadStreamFrom returns a lazy iterator over the events in the stream
// identified by id, starting at the position identified by version. An
// [eventsourcing.Revision] starts at that index; [eventsourcing.NoStream]
// requires the stream not to exist and returns a non-nil error if it does;
// [eventsourcing.StreamExists] requires that it does exist and returns a
// non-nil error if it does not; any other [eventsourcing.StreamState],
// including [eventsourcing.Any], reads from the beginning of the stream. It
// also returns a non-nil error if the requested revision is beyond the
// stream's current length.
func (m *EventStore) LoadStreamFrom(ctx context.Context, id string, version eventsourcing.StreamState) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {

	m.mu.RLock()
	events, exists := m.events[id]
	m.mu.RUnlock()

	var offset uint64

	switch v := version.(type) {
	case eventsourcing.NoStream:
		if exists {
			return nil, fmt.Errorf(
				"load stream %q: expected empty stream: %w",
				id, eventsourcing.ErrStreamExists,
			)
		}
	case eventsourcing.StreamExists:
		if !exists {
			return nil, fmt.Errorf(
				"load stream %q: expected existing stream: %w",
				id, eventsourcing.ErrStreamNotFound,
			)
		}
	case eventsourcing.Revision:
		// A start index equal to the stream's length is valid: it means
		// "anything newer than what I've already seen", the same half-open
		// convention Save relies on when it accepts Revision(currentVersion).
		if v > eventsourcing.Revision(len(events)) {
			return nil, fmt.Errorf(
				"load stream %q: requested %d but stream has %d: %w",
				id, version, len(events), eventsourcing.ErrInvalidRevision,
			)
		}
		offset = uint64(v)
	default:
	}

	// The snapshot is just a slice, so there is nothing to release.
	iter := eventsourcing.NewIteratorFunc(ctx, func(context.Context) (*eventsourcing.Envelope, error) {
		if offset >= uint64(len(events)) {
			return nil, io.EOF
		}
		rec := events[offset]
		offset++
		env, err := rec.envelope()
		if err != nil {
			return nil, fmt.Errorf("load stream %q: %w", id, err)
		}
		return env, nil
	}, nil)

	return iter, nil
}

// LoadCorrelation implements [eventsourcing.CorrelationLoader]. It scans
// the global log for the chain's events under the read lock, then decodes
// them as the caller iterates.
func (m *EventStore) LoadCorrelation(ctx context.Context, id string) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	if id == "" {
		return nil, fmt.Errorf("load correlation: %w", eventsourcing.ErrEmptyCorrelationID)
	}

	m.mu.RLock()
	var chain []*record
	for _, rec := range m.global {
		if rec.env.CorrelationID == id {
			chain = append(chain, rec)
		}
	}
	m.mu.RUnlock()

	var offset int
	return eventsourcing.NewIteratorFunc(ctx, func(context.Context) (*eventsourcing.Envelope, error) {
		if offset >= len(chain) {
			return nil, io.EOF
		}
		rec := chain[offset]
		offset++
		env, err := rec.envelope()
		if err != nil {
			return nil, fmt.Errorf("load correlation %q: %w", id, err)
		}
		return env, nil
	}, nil), nil
}

// Close discards all stored events. After Close, Save returns an error
// instead of appending. Calling Close more than once is a no-op.
func (m *EventStore) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil
	}
	m.closed = true

	m.events = make(map[string][]*record)
	m.global = make([]*record, 0)
	return nil
}

// NewEventStore constructs an empty [EventStore].
func NewEventStore() *EventStore {
	return &EventStore{
		events: make(map[string][]*record),
		global: make([]*record, 0),
	}
}
