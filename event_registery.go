package eventsourcing

import (
	"fmt"
	"sync"
)

var (
	// registry maps event names to their zero-value factory functions.
	registry = map[string]func() Event{}
	// mu protects access to the registry for concurrent operations.
	mu sync.RWMutex

	// RegisterEventByType registers an event using its type name.
	// This function can be overridden to provide custom registration behavior.
	RegisterEventByType func(ev Event) = func(ev Event) {
		registerEventTypeDefault(ev)
	}

	// RegisterEventByName registers an event with a custom name.
	// This function can be overridden to provide custom registration behavior.
	RegisterEventByName func(name string, ev Event) = func(name string, ev Event) {
		registerEventNameDefault(name, ev)
	}
	// NewEventByName creates a new instance of a registered event by its name.
	// This function can be overridden to provide custom event creation logic.
	NewEventByName func(name string) (Event, error) = newEventByNameDefault
)

// registerEventTypeDefault registers an event using its derived type name.
// Panics if the event name is already registered.
func registerEventTypeDefault[T Event](ev T) {
	eventName := ev.EventType()
	mu.Lock()
	defer mu.Unlock()

	if _, exists := registry[eventName]; exists {
		panic(fmt.Sprintf("event already registered: %s", eventName))
	}

	registry[eventName] = func() Event {
		var z T
		return z
	}
}

// registerEventNameDefault registers an event with a custom name.
// Panics if the name is already registered.
func registerEventNameDefault[T Event](name string, _ T) {
	mu.Lock()
	defer mu.Unlock()

	if _, exists := registry[name]; exists {
		panic(fmt.Sprintf("event already registered: %s", name))
	}

	registry[name] = func() Event {
		var z T
		return z
	}
}

// newEventByNameDefault creates a new zero-value instance of a registered event by name.
// Returns an error if the event is not registered.
func newEventByNameDefault(name string) (Event, error) {
	mu.RLock()
	factory, ok := registry[name]
	mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("event not registered: %s", name)
	}
	return factory(), nil
}
