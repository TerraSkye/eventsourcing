package eventsourcing

import (
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Event is a domain event describing a change that has happened to an aggregate.
type Event interface {
	AggregateID() string
}

type Envelope struct {
	EventID    uuid.UUID
	StreamID   string
	Metadata   map[string]any
	Event      Event
	Version    uint64
	OccurredAt time.Time
}

func RegisterEventType[T Event](ev T) {
	eventName := TypeName(ev)

	mu.Lock()
	defer mu.Unlock()

	registry[eventName] = func() Event {
		var z T
		return z
	}
}

// NewEventByName("UserCreated")
func NewEventByName(name string) (Event, error) {
	mu.RLock()
	factory, ok := registry[name]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("event not registered: %s", name)
	}
	// Create new zero-value instance
	return factory(), nil
}

var (
	registry = map[string]func() Event{}
	mu       sync.RWMutex
)
