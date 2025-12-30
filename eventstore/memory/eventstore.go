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

type MemoryStore struct {
	tracer trace.Tracer
	mu     sync.RWMutex
	bus    chan *eventsourcing.Envelope
	global []*eventsourcing.Envelope
	events map[string][]*eventsourcing.Envelope
}

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

func (m *MemoryStore) LoadStream(ctx context.Context, id string) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	return m.LoadStreamFrom(ctx, id, eventsourcing.StreamExists{})
}

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

func (m *MemoryStore) Events() <-chan *eventsourcing.Envelope {
	return m.bus
}

func (m *MemoryStore) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = make(map[string][]*eventsourcing.Envelope)
	close(m.bus)
	return nil
}

func NewMemoryStore(buffer int64) *MemoryStore {
	return &MemoryStore{
		events: make(map[string][]*eventsourcing.Envelope),
		global: make([]*eventsourcing.Envelope, 0),
		bus:    make(chan *eventsourcing.Envelope, buffer),
	}
}
