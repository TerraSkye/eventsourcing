package memory

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/terraskye/eventsourcing"

	"go.opentelemetry.io/otel/trace"
)

var (
	_ eventsourcing.EventStore   = (*MemoryStore)(nil)
	_ eventsourcing.GlobalReader = (*MemoryStore)(nil)
)

// MemoryStore is an in-memory [eventsourcing.EventStore] intended for tests
// and local development. It keeps every stream and the global event log in a
// single process's memory, so its contents do not survive a restart and it
// cannot be shared across processes. It is safe for concurrent use.
type MemoryStore struct {
	tracer trace.Tracer
	mu     sync.RWMutex
	closed bool
	bus    chan *eventsourcing.Envelope
	global []*eventsourcing.Envelope
	events map[string][]*eventsourcing.Envelope
}

// ReadAll implements [eventsourcing.GlobalReader]. Events are kept in a
// single slice in append order and GlobalVersion is their index plus one,
// so a read is a slice of that slice, filtered in Go.
func (m *MemoryStore) ReadAll(ctx context.Context, after uint64, opts eventsourcing.ReadOptions) (eventsourcing.ReadResult, error) {
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
		env := m.global[i]
		res.Next = env.GlobalVersion
		if matches(env, types, opts.StreamPrefixes) {
			cp := *env
			res.Events = append(res.Events, &cp)
			if len(res.Events) == limit {
				break
			}
		}
	}
	res.AtEnd = res.Next >= uint64(len(m.global))
	return res, nil
}

func matches(env *eventsourcing.Envelope, types map[string]bool, prefixes []string) bool {
	if len(types) > 0 && !types[env.Event.EventType()] && !anyIn(eventsourcing.EventNamesFor(env.Event), types) {
		return false
	}
	if len(prefixes) == 0 {
		return true
	}
	for _, p := range prefixes {
		if strings.HasPrefix(env.StreamID, p) {
			return true
		}
	}
	return false
}

func anyIn(names []string, set map[string]bool) bool {
	for _, n := range names {
		if set[n] {
			return true
		}
	}
	return false
}

// Head implements [eventsourcing.GlobalReader].
func (m *MemoryStore) Head(ctx context.Context) (uint64, error) {
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
func (m *MemoryStore) Save(ctx context.Context, events []eventsourcing.Envelope, revision eventsourcing.StreamState) (eventsourcing.AppendResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return eventsourcing.AppendResult{Successful: false}, errors.New("save events: event store is closed")
	}

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

	// Append events. events belongs to the caller, who may reuse its
	// backing array once Save returns, so the store keeps its own copy of
	// each envelope and publishes another on Events().
	for i := range events {
		ev := events[i]
		ev.GlobalVersion = uint64(len(m.global)) + 1
		m.events[streamId] = append(m.events[streamId], &ev)
		m.global = append(m.global, &ev)
		currentVersion++

		published := ev
		select {
		case m.bus <- &published:
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
		ev := events[offset]
		offset++
		return ev, nil
	}, nil)

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
// Events. After Close, Save returns an error instead of appending. Calling
// Close more than once is a no-op.
func (m *MemoryStore) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil
	}
	m.closed = true

	m.events = make(map[string][]*eventsourcing.Envelope)
	m.global = make([]*eventsourcing.Envelope, 0)
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
