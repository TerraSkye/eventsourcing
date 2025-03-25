package eventsourcing

import "github.com/google/uuid"

type Command interface {
	AggregateID() uuid.UUID
}
