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

// Envelope wraps an [Event] with the metadata an [EventStore] or [EventBus]
// needs to persist, order, and route it.
type Envelope struct {
	// EventID uniquely identifies this occurrence of the event.
	EventID uuid.UUID
	// StreamID is the ID of the stream the event was appended to, typically
	// the aggregate's ID.
	StreamID string
	// Metadata carries caller-supplied, out-of-band information about the
	// event, such as correlation IDs or the acting user.
	Metadata map[string]any
	// Event is the wrapped domain event.
	Event Event
	// Version is the event's position within its own stream, starting at 1.
	Version uint64
	// GlobalVersion is the event's position across all streams, used to
	// resume a global subscription from a specific point.
	GlobalVersion uint64
	// OccurredAt is when the event was appended to the store.
	OccurredAt time.Time
}
