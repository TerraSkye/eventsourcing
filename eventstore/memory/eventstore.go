package memory

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/terraskye/eventsourcing"

	"go.opentelemetry.io/otel/trace"
)

type MemoryStore struct {
	tracer trace.Tracer
	mu     sync.RWMutex
	bus    chan *eventsourcing.Envelope
	global []*eventsourcing.Envelope
	events map[string][]*eventsourcing.Envelope
}

func (m *MemoryStore) LoadFromAll(ctx context.Context, version uint64) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {

	m.mu.RLock()
	defer m.mu.RUnlock()

	allEvents := m.global // global slice of all events

	iter := eventsourcing.NewSliceIterator(allEvents)

	return iter, nil
}

func (m *MemoryStore) Save(ctx context.Context, events []eventsourcing.Envelope, revision eventsourcing.Revision) (eventsourcing.AppendResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(events) == 0 {
		return eventsourcing.AppendResult{Successful: true, NextExpectedVersion: 0}, nil
	}

	streamId := events[0].StreamID
	currentVersion := uint64(len(m.events[streamId]))

	// Handle revision enforcement
	switch rev := revision.(type) {
	case eventsourcing.Any:
		// No concurrency check
	case eventsourcing.NoStream:
		if currentVersion != 0 {
			err := fmt.Errorf("stream already exists for aggregate %s", streamId)
			return eventsourcing.AppendResult{Successful: false}, err
		}
	case eventsourcing.StreamExists:
		if currentVersion == 0 {
			err := fmt.Errorf("stream does not exist for aggregate %s", streamId)
			return eventsourcing.AppendResult{Successful: false}, err
		}
	case eventsourcing.ExplicitRevision:
		if currentVersion != uint64(rev) {
			return eventsourcing.AppendResult{}, eventsourcing.StreamRevisionConflictError{
				Stream:           streamId,
				ExpectedRevision: uint64(rev),
				ActualRevision:   currentVersion,
			}

		}
	default:
		err := fmt.Errorf("unsupported revision type for aggregate %s", streamId)
		return eventsourcing.AppendResult{Successful: false}, err
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
		Successful:          true,
		NextExpectedVersion: currentVersion,
	}, nil
}

func (m *MemoryStore) LoadStream(ctx context.Context, u string) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {

	m.mu.RLock()
	events, exists := m.events[u]
	m.mu.RUnlock()

	if !exists {
		// Return empty sequence
		return eventsourcing.NewIteratorFunc(func(ctx context.Context) (*eventsourcing.Envelope, error) {
			return nil, io.EOF
		}), nil
	}

	index := 0
	iter := eventsourcing.NewIteratorFunc(func(ctx context.Context) (*eventsourcing.Envelope, error) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if index >= len(events) {
			return nil, io.EOF
		}
		ev := events[index]
		index++
		return ev, nil
	})
	return iter, nil
}

func (m *MemoryStore) LoadStreamFrom(ctx context.Context, id string, version uint64) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {

	m.mu.RLock()
	events, exists := m.events[id]
	m.mu.RUnlock()

	if !exists || int(version) >= len(events) {
		// Return empty sequence
		return eventsourcing.NewIteratorFunc(func(ctx context.Context) (*eventsourcing.Envelope, error) { return nil, io.EOF }), nil
	}

	index := version

	iter := eventsourcing.NewIteratorFunc(func(ctx context.Context) (*eventsourcing.Envelope, error) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if int(index) >= len(events) {
			return nil, io.EOF
		}
		ev := events[index]
		index++
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
