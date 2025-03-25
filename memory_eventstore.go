package eventsourcing

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"sync"
)

type MemoryStore struct {
	tracer trace.Tracer
	mu     sync.RWMutex
	bus    EventBus
	events map[uuid.UUID][]*Envelope
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
		span.AddEvent("Stored event",
			trace.WithAttributes(
				attribute.String("event.aggregate_id", event.Event.AggregateID().String()),
				attribute.String("event.type", TypeName(event.Event)),
				attribute.Int("version", int(originalVersion)),
			),
		)
		originalVersion++

	}

	for _, event := range events {

		eventCtx, eventSpan := m.tracer.Start(ctx, " cqrs.event.store.publish",
			trace.WithAttributes(
				attribute.String("event.aggregate_id", event.Event.AggregateID().String()),
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

func (m *MemoryStore) Load(ctx context.Context, u uuid.UUID) (<-chan *Envelope, error) {
	ctx, span := m.tracer.Start(ctx, "MemoryStore.Load",
		trace.WithAttributes(attribute.String("aggregate_id", u.String())),
	)
	defer span.End()

	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make(chan *Envelope, 10)
	go func() {
		defer close(out)
		if events, exists := m.events[u]; exists {
			for _, event := range events {
				select {
				case <-ctx.Done():
					return
				case out <- event:
				}
			}
		}
	}()

	return out, nil
}

func (m *MemoryStore) LoadFrom(ctx context.Context, id uuid.UUID, version int) (<-chan *Envelope, error) {
	ctx, span := m.tracer.Start(ctx, "MemoryStore.LoadFrom",
		trace.WithAttributes(
			attribute.String("aggregate_id", id.String()),
			attribute.Int("start_version", version),
		),
	)
	defer span.End()

	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make(chan *Envelope, 10)
	go func() {
		defer close(out)
		if events, exists := m.events[id]; exists && version < len(events) {
			for _, event := range events[version:] {
				select {
				case <-ctx.Done():
					span.RecordError(ctx.Err())
					span.SetStatus(codes.Error, ctx.Err().Error())
					return
				case out <- event:
				}
			}
		}
	}()
	span.SetStatus(codes.Ok, "")
	return out, nil
}

func (m *MemoryStore) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = make(map[uuid.UUID][]*Envelope)
	return nil
}

func NewMemoryStore(bus EventBus) EventStore {
	return &MemoryStore{
		events: make(map[uuid.UUID][]*Envelope),
		tracer: otel.Tracer("event-store"),
		bus:    bus,
	}
}
