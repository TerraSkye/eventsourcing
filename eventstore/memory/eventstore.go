package memory

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/terraskye/eventsourcing"

	"go.opentelemetry.io/otel/trace"
)

var _ eventsourcing.EventStore = (*MemoryStore)(nil)

// MemoryStore is an in-memory [eventsourcing.EventStore] intended for tests
// and local development. It keeps every stream and the global event log in a
// single process's memory, so its contents do not survive a restart and it
// cannot be shared across processes. It is safe for concurrent use.
type MemoryStore struct {
	tracer trace.Tracer
	mu     sync.RWMutex
	bus    chan *eventsourcing.Envelope
	global []*eventsourcing.Envelope
	events map[string][]*eventsourcing.Envelope
}

// LoadFromAll returns a lazy iterator over every event ever saved, across all
// streams, in the order they were appended, starting at the position
// identified by version. version is expected to be an
// [eventsourcing.Revision] (or [eventsourcing.NoStream], which behaves like
// revision 0); it returns a non-nil error if that position is beyond the
// number of events currently stored.
func (m *MemoryStore) LoadFromAll(ctx context.Context, version eventsourcing.StreamState) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	allEvents := m.global // global slice of all events

	if int(version.ToRawInt64()) >= len(allEvents) {
		return nil, fmt.Errorf(
			"load stream %q: requested %d but stream has %d: %w",
			"all", version, len(allEvents), eventsourcing.ErrInvalidRevision,
		)
	}

	// TODO: unlike LoadStreamFrom, this does not type-switch on version, so
	// eventsourcing.Any.ToRawInt64() (-1) and eventsourcing.StreamExists.ToRawInt64()
	// (-2) are cast directly to uint64 below, producing a huge offset that
	// indexes allEvents out of range and panics instead of returning an
	// error (confirmed: LoadFromAll(ctx, eventsourcing.Any{}) panics with
	// "index out of range" on the first Next() call, even against an empty
	// store). Only eventsourcing.Revision and eventsourcing.NoStream (0)
	// currently work correctly.
	var offset = uint64(version.ToRawInt64())

	iter := eventsourcing.NewIteratorFunc(func(ctx context.Context) (*eventsourcing.Envelope, error) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if int(offset) >= len(allEvents) {
			return nil, io.EOF
		}
		ev := allEvents[offset]
		offset++
		return ev, nil
	})

	return iter, nil
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
func (m *MemoryStore) Save(ctx context.Context, events []eventsourcing.Envelope, revision eventsourcing.StreamState) (eventsourcing.AppendResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

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

	// Append events
	for i := range events {
		m.events[streamId] = append(m.events[streamId], &events[i])
		m.global = append(m.global, &events[i])
		currentVersion++

		select {
		case m.bus <- &events[i]:
		default:
			// Drop error if channel full
		}
	}

	return eventsourcing.AppendResult{
		StreamID:            streamId,
		Successful:          true,
		NextExpectedVersion: currentVersion,
	}, nil
}

// LoadStream returns a lazy iterator over all events in the stream
// identified by id, in the order they were appended. It returns a non-nil
// error if the stream does not exist.
func (m *MemoryStore) LoadStream(ctx context.Context, id string) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
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
func (m *MemoryStore) LoadStreamFrom(ctx context.Context, id string, version eventsourcing.StreamState) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {

	m.mu.RLock()
	events, exists := m.events[id]
	m.mu.RUnlock()

	var offset uint64

	switch version.(type) {
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
		if int(version.ToRawInt64()) >= len(events) {
			return nil, fmt.Errorf(
				"load stream %q: requested %d but stream has %d: %w",
				id, version, len(events), eventsourcing.ErrInvalidRevision,
			)
		}
		offset = uint64(version.ToRawInt64())
	default:
	}

	iter := eventsourcing.NewIteratorFunc(func(ctx context.Context) (*eventsourcing.Envelope, error) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if int(offset) >= len(events) {
			return nil, io.EOF
		}
		ev := events[offset]
		offset++
		return ev, nil
	})

	return iter, nil
}

// Events returns a channel that receives every [eventsourcing.Envelope] as it
// is saved. It is not part of the [eventsourcing.EventStore] interface; it
// exists so tests and local tooling can observe writes as they happen. The
// channel has the buffer size passed to [NewMemoryStore] and sends are
// non-blocking, so a slow consumer misses events rather than blocking Save.
// The channel is closed by Close.
func (m *MemoryStore) Events() <-chan *eventsourcing.Envelope {
	return m.bus
}

// Close discards all stored events and closes the channel returned by
// Events. After Close, the MemoryStore must not be used again.
//
// TODO: this is not idempotent, contrary to the eventsourcing.EventStore
// contract, which asks implementations to make Close safe to call more than
// once — a second call panics with "close of closed channel" (confirmed).
func (m *MemoryStore) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = make(map[string][]*eventsourcing.Envelope)
	close(m.bus)
	return nil
}

// NewMemoryStore constructs an empty [MemoryStore]. buffer sets the capacity
// of the channel returned by [MemoryStore.Events].
func NewMemoryStore(buffer int64) *MemoryStore {
	return &MemoryStore{
		events: make(map[string][]*eventsourcing.Envelope),
		global: make([]*eventsourcing.Envelope, 0),
		bus:    make(chan *eventsourcing.Envelope, buffer),
	}
}
