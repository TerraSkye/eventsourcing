package eventsourcing

import (
	"github.com/google/uuid"
	"time"
)

var now = time.Now

// Aggregate is the interface that all aggregates must implement.
type Aggregate interface {

	// EntityID returns the unique identifier of the aggregate.
	EntityID() string

	// AggregateVersion returns the version of the aggregate.
	AggregateVersion() uint64

	// SetAggregateVersion sets the version of the aggregate.
	SetAggregateVersion(version uint64)

	// UncommittedEvents returns all the events that are currently uncommitted.
	UncommittedEvents() []Envelope

	// ClearUncommittedEvents clears all uncommitted events from the aggregate.
	ClearUncommittedEvents()

	// AppendEvent appends a new event to the aggregate's event list.
	AppendEvent(event Event, options ...EventOption)
}

type AggregateBase struct {
	id     string
	v      uint64
	events []Envelope
}

// NewAggregateBase creates an aggregate.
func NewAggregateBase(id string) *AggregateBase {
	return &AggregateBase{
		id:     id,
		events: make([]Envelope, 0),
	}
}

// EntityID implements the EntityID method of the eh.Entity and eh.Aggregate interface.
func (a *AggregateBase) EntityID() string {
	return a.id
}

// AggregateVersion implements the AggregateVersion method of the Aggregate interface.
func (a *AggregateBase) AggregateVersion() uint64 {
	return a.v
}

// SetAggregateVersion implements the SetAggregateVersion method of the Aggregate interface.
func (a *AggregateBase) SetAggregateVersion(v uint64) {
	a.v = v
}

// UncommittedEvents implements the UncommittedEvents method of the eh.EventSource
// interface.
func (a *AggregateBase) UncommittedEvents() []Envelope {
	return a.events
}

// ClearUncommittedEvents implements the ClearUncommittedEvents method of the eh.EventSource
// interface.
func (a *AggregateBase) ClearUncommittedEvents() {
	a.events = nil
}

// AppendEvent appends an event for later retrieval by Events().
func (a *AggregateBase) AppendEvent(event Event, options ...EventOption) {

	envelope := Envelope{
		UUID:       uuid.New(),
		Metadata:   make(map[string]any),
		Event:      event,
		Version:    a.AggregateVersion() + uint64(len(a.events)) + 1,
		OccurredAt: now(),
	}

	for _, option := range options {
		option(&envelope)
	}

	a.events = append(a.events, envelope)
}
