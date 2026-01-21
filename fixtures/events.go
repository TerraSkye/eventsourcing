package fixtures

import (
	"fmt"

	es "github.com/terraskye/eventsourcing"
)

// TestEvent is a configurable test event implementing the Event interface.
type TestEvent struct {
	ID   string
	Type string
	Data string
}

func (e TestEvent) AggregateID() string { return e.ID }
func (e TestEvent) EventType() string   { return e.Type }

// TestEventBuilder provides a fluent API for constructing test events.
type TestEventBuilder struct {
	id       string
	typ      string
	data     string
	sequence int
}

// NewTestEvent creates a new TestEventBuilder with sensible defaults.
func NewTestEvent() *TestEventBuilder {
	return &TestEventBuilder{
		id:   "aggregate-1",
		typ:  "TestEvent",
		data: "",
	}
}

// WithID sets the aggregate ID.
func (b *TestEventBuilder) WithID(id string) *TestEventBuilder {
	b.id = id
	return b
}

// WithType sets the event type.
func (b *TestEventBuilder) WithType(typ string) *TestEventBuilder {
	b.typ = typ
	return b
}

// WithData sets custom data on the event.
func (b *TestEventBuilder) WithData(data string) *TestEventBuilder {
	b.data = data
	return b
}

// Build constructs the TestEvent.
func (b *TestEventBuilder) Build() TestEvent {
	return TestEvent{
		ID:   b.id,
		Type: b.typ,
		Data: b.data,
	}
}

// BuildN creates n events with sequential data.
func (b *TestEventBuilder) BuildN(n int) []es.Event {
	events := make([]es.Event, n)
	for i := 0; i < n; i++ {
		events[i] = TestEvent{
			ID:   b.id,
			Type: b.typ,
			Data: fmt.Sprintf("%s-%d", b.data, i+1),
		}
	}
	return events
}

// Common pre-built events for quick testing.
var (
	OrderCreatedEvent = TestEvent{ID: "order-1", Type: "OrderCreated", Data: ""}
	OrderUpdatedEvent = TestEvent{ID: "order-1", Type: "OrderUpdated", Data: ""}
	OrderDeletedEvent = TestEvent{ID: "order-1", Type: "OrderDeleted", Data: ""}

	UserCreatedEvent = TestEvent{ID: "user-1", Type: "UserCreated", Data: ""}
	UserUpdatedEvent = TestEvent{ID: "user-1", Type: "UserUpdated", Data: ""}
)

// DomainEvent is a more realistic test event with additional fields.
type DomainEvent struct {
	AggID     string
	EventName string
	Payload   map[string]any
}

func (e DomainEvent) AggregateID() string { return e.AggID }
func (e DomainEvent) EventType() string   { return e.EventName }

// DomainEventBuilder provides a fluent API for constructing domain events.
type DomainEventBuilder struct {
	aggID     string
	eventName string
	payload   map[string]any
}

// NewDomainEvent creates a new DomainEventBuilder.
func NewDomainEvent() *DomainEventBuilder {
	return &DomainEventBuilder{
		aggID:     "aggregate-1",
		eventName: "DomainEvent",
		payload:   make(map[string]any),
	}
}

// WithAggregateID sets the aggregate ID.
func (b *DomainEventBuilder) WithAggregateID(id string) *DomainEventBuilder {
	b.aggID = id
	return b
}

// WithEventName sets the event name/type.
func (b *DomainEventBuilder) WithEventName(name string) *DomainEventBuilder {
	b.eventName = name
	return b
}

// WithPayload sets the entire payload.
func (b *DomainEventBuilder) WithPayload(payload map[string]any) *DomainEventBuilder {
	b.payload = payload
	return b
}

// WithField adds a single field to the payload.
func (b *DomainEventBuilder) WithField(key string, value any) *DomainEventBuilder {
	b.payload[key] = value
	return b
}

// Build constructs the DomainEvent.
func (b *DomainEventBuilder) Build() DomainEvent {
	return DomainEvent{
		AggID:     b.aggID,
		EventName: b.eventName,
		Payload:   b.payload,
	}
}
