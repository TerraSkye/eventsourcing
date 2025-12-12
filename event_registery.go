package eventsourcing

import (
	"fmt"
	"sync"
)

var (
	// registry maps event names to their factory functions.
	// Each factory must return a new instance of a concrete Event type.
	registry = map[string]func() Event{}

	// mu protects access to the registry for concurrent operations.
	mu sync.RWMutex

	// RegisterEventByType registers a new Event type using its default type name.
	//
	// It provides a reusable pattern for dynamically creating new event instances
	// by string name. Registration performs the following steps:
	//   1. Calls the provided factory function to obtain an instance of the event.
	//   2. Retrieves the type name using EventType().
	//   3. Registers the factory in the registry keyed by the type name.
	//
	// Parameters:
	//   - fn: A factory function of type func() Event that returns a new instance
	//     of the event. The factory must not return nil.
	//
	// Panics:
	//   - If the factory function is nil.
	//   - If the factory returns nil.
	//   - If an event with the same type name is already registered.
	//
	// Example Usage:
	//   RegisterEventByType(func() Event { return &InventoryChanged{} })
	RegisterEventByType func(fn func() Event) = func(fn func() Event) {
		registerEventNameDefault(fn().EventType(), fn)
	}

	// RegisterEventByName registers a new Event type under a custom name.
	//
	// This is similar to RegisterEventByType, but allows using a name
	// that is independent of EventType(). The provided factory function
	// must return a new instance of the event type each time it is called.
	//
	// Parameters:
	//   - name: The unique name to register the event under.
	//   - fn: Factory function of type func() Event that returns a new instance.
	//
	// Panics:
	//   - If fn is nil.
	//   - If fn returns nil.
	//   - If the name is already registered.
	//
	// Example Usage:
	//   RegisterEventByName("CustomEventName", func() Event { return &InventoryChanged{} })
	RegisterEventByName func(name string, fn func() Event) = func(name string, fn func() Event) {
		registerEventNameDefault(name, fn)
	}

	// NewEventByName creates a new instance of a registered Event by its name.
	//
	// This function allows dynamic instantiation of events using their string name.
	// It performs the following steps:
	//   1. Looks up the factory function in the registry.
	//   2. Calls the factory to create a new instance.
	//
	// Parameters:
	//   - name: The name of the event to create.
	//
	// Returns:
	//   - Event: A new instance of the registered event.
	//   - error: Non-nil if the event name is not registered or the factory
	//     returned nil.
	//
	// Example Usage:
	//   ev, err := NewEventByName("InventoryChanged")
	NewEventByName func(name string) (Event, error) = newEventByNameDefault
)

// registerEventNameDefault is the internal implementation for registering an event under a name.
//
// It validates the factory function, ensures uniqueness, and stores the factory.
func registerEventNameDefault(name string, fn func() Event) {
	if fn == nil {
		panic("cannot register nil factory")
	}

	mu.Lock()
	defer mu.Unlock()

	if _, exists := registry[name]; exists {
		panic(fmt.Sprintf("event already registered: %s", name))
	}

	ev := fn()
	if ev == nil {
		panic(fmt.Sprintf("factory returned nil for event: %s", name))
	}

	registry[name] = fn
}

// newEventByNameDefault is the internal implementation of NewEventByName.
//
// It retrieves the factory for the given name and returns a new instance.
// Returns an error if the event is not registered or the factory returns nil.
func newEventByNameDefault(name string) (Event, error) {
	mu.RLock()
	factory, ok := registry[name]
	mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("event not registered: %s", name)
	}
	ev := factory()
	if ev == nil {
		return nil, fmt.Errorf("factory returned nil for event: %s", name)
	}
	return ev, nil
}
