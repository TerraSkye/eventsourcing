package eventsourcing

import (
	"context"
	"github.com/google/uuid"
	"time"
)

type Event interface {
	AggregateID() string
}

type Envelope struct {
	UUID       uuid.UUID
	Metadata   map[string]any
	Event      Event
	Version    uint64
	OccurredAt time.Time
}

type EventOption func(e *Envelope)

func WithMetaData(ctx context.Context) EventOption {
	return func(e *Envelope) {
		e.Metadata = map[string]any{}
	}
}
