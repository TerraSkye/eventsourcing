package memory

import (
	"context"
	"fmt"
	"github.com/terraskye/eventsourcing"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type memoryStore struct {
	tracer trace.Tracer
	mu     sync.RWMutex
	//bus    EventBus
	global []*eventsourcing.Envelope
	events map[string][]*eventsourcing.Envelope
}

func (m *memoryStore) LoadFromAll(ctx context.Context, version uint64) (*eventsourcing.EnvelopeIterator, error) {
	ctx, span := m.tracer.Start(ctx, "memoryStore.LoadFromAll",
		trace.WithAttributes(
			attribute.Int64("start_version", int64(version)),
		),
	)
	defer span.End()

	m.mu.RLock()
	defer m.mu.RUnlock()

	index := version
	allEvents := m.global // global slice of all events

	iter := eventsourcing.NewIterator(func(ctx context.Context) (*eventsourcing.Envelope, error) {
		if ctx.Err() != nil {
			span.RecordError(ctx.Err())
			span.SetStatus(codes.Error, ctx.Err().Error())
			return nil, ctx.Err()
		}
		if int(index) >= len(allEvents) {
			return nil, nil
		}
		ev := allEvents[index]
		index++
		return ev, nil
	})

	return iter, nil
}

func (m *memoryStore) Save(ctx context.Context, events []eventsourcing.Envelope, revision eventsourcing.Revision) (eventsourcing.AppendResult, error) {
	ctx, span := m.tracer.Start(ctx, "cqrs.event.store.write",
		trace.WithAttributes(attribute.Int("event.count", len(events))),
	)
	defer span.End()

	m.mu.Lock()
	defer m.mu.Unlock()

	if len(events) == 0 {
		return eventsourcing.AppendResult{Successful: true, NextExpectedVersion: 0}, nil
	}

	aggregateID := events[0].Event.AggregateID()
	currentVersion := uint64(len(m.events[aggregateID]))

	// Handle revision enforcement
	switch rev := revision.(type) {
	case eventsourcing.Any:
		// No concurrency check
	case eventsourcing.NoStream:
		if currentVersion != 0 {
			err := fmt.Errorf("stream already exists for aggregate %s", aggregateID)
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return eventsourcing.AppendResult{Successful: false}, err
		}
	case eventsourcing.StreamExists:
		if currentVersion == 0 {
			err := fmt.Errorf("stream does not exist for aggregate %s", aggregateID)
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return eventsourcing.AppendResult{Successful: false}, err
		}
	case eventsourcing.ExplicitRevision:
		if currentVersion != uint64(rev) {
			err := fmt.Errorf("version mismatch for aggregate %s: expected %d, got %d", aggregateID, rev, currentVersion)
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return eventsourcing.AppendResult{Successful: false}, err
		}
	default:
		err := fmt.Errorf("unsupported revision type for aggregate %s", aggregateID)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return eventsourcing.AppendResult{Successful: false}, err
	}

	// Append events
	for i := range events {
		m.events[aggregateID] = append(m.events[aggregateID], &events[i])
		m.global = append(m.global, &events[i])

		span.AddEvent("Stored event",
			trace.WithAttributes(
				attribute.String("event.aggregate_id", aggregateID),
				attribute.String("event.type", eventsourcing.TypeName(events[i].Event)),
				attribute.Int("version", int(currentVersion)),
			),
		)

		currentVersion++
	}

	// Publish events
	//for _, event := range events {
	//	eventCtx, eventSpan := m.tracer.Start(ctx, "cqrs.event.store.publish",
	//		trace.WithAttributes(
	//			attribute.String("event.aggregate_id", event.Event.AggregateID()),
	//			attribute.String("event.type", TypeName(event.Event)),
	//		),
	//	)
	//
	//	//if err := m.bus.Dispatch(eventCtx, event.Event); err != nil {
	//	//	eventSpan.RecordError(err)
	//	//	eventSpan.SetStatus(codes.Error, err.Error())
	//	//	eventSpan.End()
	//	//	return AppendResult{
	//	//		Successful: false,
	//	//	}, err
	//	//}
	//
	//	eventSpan.End()
	//}

	return eventsourcing.AppendResult{
		Successful:          true,
		NextExpectedVersion: currentVersion,
	}, nil
}
func (m *memoryStore) LoadStream(ctx context.Context, u string) (*eventsourcing.EnvelopeIterator, error) {
	ctx, span := m.tracer.Start(ctx, "memoryStore.Load",
		trace.WithAttributes(attribute.String("aggregate_id", u)),
	)
	defer span.End()

	m.mu.RLock()
	events, exists := m.events[u]
	m.mu.RUnlock()

	if !exists {
		// Return empty sequence
		return eventsourcing.NewIterator(func(ctx context.Context) (*eventsourcing.Envelope, error) {
			return nil, nil
		}), nil
	}

	index := 0
	iter := eventsourcing.NewIterator(func(ctx context.Context) (*eventsourcing.Envelope, error) {
		if ctx.Err() != nil {
			span.RecordError(ctx.Err())
			span.SetStatus(codes.Error, ctx.Err().Error())
			return nil, ctx.Err()
		}
		if index >= len(events) {
			return nil, nil
		}
		ev := events[index]
		index++
		return ev, nil
	})
	return iter, nil
}

func (m *memoryStore) LoadStreamFrom(ctx context.Context, id string, version uint64) (*eventsourcing.EnvelopeIterator, error) {
	ctx, span := m.tracer.Start(ctx, "memoryStore.LoadFrom",
		trace.WithAttributes(
			attribute.String("aggregate_id", id),
			attribute.Int("start_version", int(version)),
		),
	)
	defer span.End()

	m.mu.RLock()
	events, exists := m.events[id]
	m.mu.RUnlock()

	if !exists || int(version) >= len(events) {
		// Return empty sequence
		return eventsourcing.NewIterator(func(ctx context.Context) (*eventsourcing.Envelope, error) { return nil, nil }), nil
	}

	index := version

	iter := eventsourcing.NewIterator(func(ctx context.Context) (*eventsourcing.Envelope, error) {
		if ctx.Err() != nil {
			span.RecordError(ctx.Err())
			span.SetStatus(codes.Error, ctx.Err().Error())
			return nil, ctx.Err()
		}
		if int(index) >= len(events) {
			return nil, nil
		}
		ev := events[index]
		index++
		return ev, nil
	})

	return iter, nil
}

func (m *memoryStore) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = make(map[string][]*eventsourcing.Envelope)
	return nil
}

func NewMemoryStore() eventsourcing.EventStore {
	return &memoryStore{
		events: make(map[string][]*eventsourcing.Envelope),
		global: make([]*eventsourcing.Envelope, 0),
		tracer: otel.Tracer("event-store"),
	}
}
