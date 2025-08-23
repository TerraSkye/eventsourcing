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
	bus    EventBus
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

func (m *MemoryStore) Save(ctx context.Context, events []Envelope, originalVersion uint64) error {
	ctx, span := m.tracer.Start(ctx, "cqrs.event.store.write",
		trace.WithAttributes(attribute.Int("event.count", len(events))),
	)
	defer span.End()

	m.mu.Lock()
	defer m.mu.Unlock()

	if len(events) == 0 {
		return nil
	}

	for i, event := range events {
		currentVersion := uint64(len(m.events[event.Event.AggregateID()]))

		if currentVersion != originalVersion {
			err := fmt.Errorf("version mismatch: expected %d, got %d", originalVersion, currentVersion)
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return err
		}
		m.events[event.Event.AggregateID()] = append(m.events[event.Event.AggregateID()], &events[i])
		m.global = append(m.global, &events[i])
		span.AddEvent("Stored event",
			trace.WithAttributes(
				attribute.String("event.aggregate_id", event.Event.AggregateID()),
				attribute.String("event.type", TypeName(event.Event)),
				attribute.Int("version", int(originalVersion)),
			),
		)
		originalVersion++

	}

	for _, event := range events {

		eventCtx, eventSpan := m.tracer.Start(ctx, " cqrs.event.store.publish",
			trace.WithAttributes(
				attribute.String("event.aggregate_id", event.Event.AggregateID()),
				attribute.String("event.type", TypeName(event.Event)),
			),
		)

		//TODO populate the context with the event envelope
		if err := m.bus.Dispatch(eventCtx, event.Event); err != nil {
			eventSpan.RecordError(err)
			eventSpan.SetStatus(codes.Error, err.Error())
			eventSpan.End()
			return err
		}

		eventSpan.End()
	}

	return nil

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

func NewMemoryStore(bus EventBus) EventStore {
	return &MemoryStore{
		events: make(map[string][]*Envelope),
		global: make([]*Envelope, 0),
		tracer: otel.Tracer("event-store"),
		bus:    bus,
	}
}
