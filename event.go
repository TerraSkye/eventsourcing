package eventsourcing

import (
	"time"

	"github.com/google/uuid"
)

// Event is a domain event describing a change that has happened to an aggregate.
type Event interface {
	AggregateID() string
	EventType() string
}

type Envelope struct {
	EventID    uuid.UUID
	StreamID   string
	Metadata   map[string]any
	Event      Event
	Version    uint64
	OccurredAt time.Time
}
