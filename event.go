package eventsourcing

import (
	"time"

	"github.com/google/uuid"
)

// Event is a domain event describing a change that has already happened to
// an aggregate.
type Event interface {
	// AggregateID returns the ID of the aggregate this event happened to.
	AggregateID() string
	// EventType returns the name under which this event's concrete type is
	// registered; see [RegisterEvent].
	EventType() string
}

// Envelope wraps an [Event] with the metadata an [EventStore] needs to
// persist and order it, and its readers need to route it.
type Envelope struct {
	// EventID uniquely identifies this occurrence of the event.
	EventID uuid.UUID
	// StreamID is the ID of the stream the event was appended to, typically
	// the aggregate's ID.
	StreamID string
	// Metadata carries caller-supplied, out-of-band information about the
	// event, such as the acting user.
	Metadata map[string]any
	// CorrelationID identifies the chain of events this one belongs to:
	// the ID of the chain's first message, copied unchanged to every event
	// it caused, directly or not. [NewCommandHandler] takes it from the
	// context, or starts a chain with the first appended event's ID when
	// the context carries none.
	CorrelationID string
	// CausationID is the EventID of the event whose handling produced this
	// one, or "" for the root of a chain: an event no event caused, such as
	// one from a command an API call sent.
	CausationID string
	// Event is the wrapped domain event.
	Event Event
	// Version is the event's position within its own stream, starting at 1.
	Version uint64
	// GlobalPosition is the event's position across all streams, used to
	// resume a global subscription from a specific point.
	GlobalPosition uint64
	// OccurredAt is when the event was appended to the store.
	OccurredAt time.Time
}
