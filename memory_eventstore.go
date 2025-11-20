package eventsourcing

import (
	"context"
	"fmt"
	"iter"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type MemoryStore struct {
	tracer trace.Tracer
	mu     sync.RWMutex
	//bus    EventBus
	global []*Envelope
	events map[string][]*Envelope
}

func (m *MemoryStore) LoadFromAll(ctx context.Context, version uint64) (iter.Seq[*Envelope], error) {
	ctx, span := m.tracer.Start(ctx, "MemoryStore.LoadFromAll",
		trace.WithAttributes(
			attribute.Int64("start_version", int64(version)),
		),
	)
	defer span.End()

	m.mu.RLock()
	defer m.mu.RUnlock()

	seq := func(yield func(*Envelope) bool) {
		for _, events := range m.events {
			if int(version) < len(events) {
				for _, event := range events[version:] {
					select {
					case <-ctx.Done():
						span.RecordError(ctx.Err())
						span.SetStatus(codes.Error, ctx.Err().Error())
						return
					default:
						if !yield(event) {
							return
						}
					}
				}
			}
		}
		span.SetStatus(codes.Ok, "")
	}

	return seq, nil
}

func (m *MemoryStore) Save(ctx context.Context, events []Envelope, revision Revision) (AppendResult, error) {
	ctx, span := m.tracer.Start(ctx, "cqrs.event.store.write",
		trace.WithAttributes(attribute.Int("event.count", len(events))),
	)
	defer span.End()

	m.mu.Lock()
	defer m.mu.Unlock()

	if len(events) == 0 {
		return AppendResult{Successful: true, NextExpectedVersion: 0}, nil
	}

	aggregateID := events[0].Event.AggregateID()
	currentVersion := uint64(len(m.events[aggregateID]))

	// Handle revision enforcement
	switch rev := revision.(type) {
	case Any:
		// No concurrency check
	case NoStream:
		if currentVersion != 0 {
			err := fmt.Errorf("stream already exists for aggregate %s", aggregateID)
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return AppendResult{Successful: false}, err
		}
	case StreamExists:
		if currentVersion == 0 {
			err := fmt.Errorf("stream does not exist for aggregate %s", aggregateID)
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return AppendResult{Successful: false}, err
		}
	case ExplicitRevision:
		if currentVersion != uint64(rev) {
			err := fmt.Errorf("version mismatch for aggregate %s: expected %d, got %d", aggregateID, rev, currentVersion)
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return AppendResult{Successful: false}, err
		}
	default:
		err := fmt.Errorf("unsupported revision type for aggregate %s", aggregateID)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return AppendResult{Successful: false}, err
	}

	// Append events
	for i := range events {
		m.events[aggregateID] = append(m.events[aggregateID], &events[i])
		m.global = append(m.global, &events[i])

		span.AddEvent("Stored event",
			trace.WithAttributes(
				attribute.String("event.aggregate_id", aggregateID),
				attribute.String("event.type", TypeName(events[i].Event)),
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

	return AppendResult{
		Successful:          true,
		NextExpectedVersion: currentVersion,
	}, nil
}
func (m *MemoryStore) LoadStream(ctx context.Context, u string) (iter.Seq[*Envelope], error) {
	ctx, span := m.tracer.Start(ctx, "MemoryStore.Load",
		trace.WithAttributes(attribute.String("aggregate_id", u)),
	)
	defer span.End()

	m.mu.RLock()
	events, exists := m.events[u]
	m.mu.RUnlock()

	if !exists {
		// Return empty sequence
		return func(yield func(*Envelope) bool) {}, nil
	}

	seq := func(yield func(*Envelope) bool) {
		for _, event := range events {
			select {
			case <-ctx.Done():
				return
			default:
				if !yield(event) {
					return
				}
			}
		}
	}

	return seq, nil
}

func (m *MemoryStore) LoadStreamFrom(ctx context.Context, id string, version uint64) (iter.Seq[*Envelope], error) {
	ctx, span := m.tracer.Start(ctx, "MemoryStore.LoadFrom",
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
		return func(yield func(*Envelope) bool) {}, nil
	}

	seq := func(yield func(*Envelope) bool) {
		for _, event := range events[version:] {
			select {
			case <-ctx.Done():
				span.RecordError(ctx.Err())
				span.SetStatus(codes.Error, ctx.Err().Error())
				return
			default:
				if !yield(event) {
					return
				}
			}
		}
		span.SetStatus(codes.Ok, "")
	}

	return seq, nil
}

func (m *MemoryStore) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = make(map[string][]*Envelope)
	return nil
}

func NewMemoryStore() EventStore {
	return &MemoryStore{
		events: make(map[string][]*Envelope),
		global: make([]*Envelope, 0),
		tracer: otel.Tracer("event-store"),
	}
}
