package eventsourcing

import (
	"context"
)

// EventStore is an interface for an event sourcing event store.
type EventStore interface {
	// Save appends all events in the event stream to the store.
	Save(ctx context.Context, events []Envelope, originalVersion uint64) error

	// Load loads all events for the aggregate id from the store.
	Load(ctx context.Context, id string) (<-chan *Envelope, error)

	// LoadFrom loads all events from version for the aggregate id from the store.
	LoadFrom(ctx context.Context, id string, version int) (<-chan *Envelope, error)

	// Close closes the EventStore.
	Close() error
}
