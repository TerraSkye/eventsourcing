package eventsourcing

import (
	"fmt"
	"sync"
)

var (
	// registry maps event names to their factory functions.
	// Each factory must return a new instance of a concrete Event type.
	registry = map[string]func() Event{}

	// typeToNames maps a concrete Event type string to the names it is registered under.
	typeToNames = map[string][]string{}

	// registryMu protects access to the registry for concurrent operations.
	registryMu sync.RWMutex

	// RegisterEventByType registers fn under the type name of the [Event] it
	// returns, i.e. fn().EventType(). Use this instead of [RegisterEvent]
	// when you need explicit control over the factory, for example to
	// inject constructor arguments. It panics if fn is nil, if fn() returns
	// nil, or if an event is already registered under that name.
	//
	// Example Usage:
	//   RegisterEventByType(func() Event { return &InventoryChanged{} })
	RegisterEventByType func(fn func() Event) = func(fn func() Event) {
		registerEventNameDefault(fn().EventType(), fn)
	}

	// RegisterEventByName registers fn under name, independently of the
	// event's own EventType() — useful when the name a store has events
	// persisted under no longer matches the current type name, for example
	// after a rename. As with [RegisterEventByType], it panics if fn is nil,
	// if fn() returns nil, or if name is already registered.
	//
	// Example Usage:
	//   RegisterEventByName("CustomEventName", func() Event { return &InventoryChanged{} })
	RegisterEventByName func(name string, fn func() Event) = func(name string, fn func() Event) {
		registerEventNameDefault(name, fn)
	}

	// NewEventByName returns a new instance of the [Event] registered under
	// name, or a non-nil [ErrEventNotRegistered] if no event is registered
	// under that name.
	//
	// Example Usage:
	//   ev, err := NewEventByName("InventoryChanged")
	NewEventByName func(name string) (Event, error) = newEventByNameDefault

	// EventNamesFor returns every name event's concrete type is registered
	// under, regardless of which [RegisterEvent]/[RegisterEventByType]/
	// [RegisterEventByName] call added each one.
	EventNamesFor func(event Event) []string = func(event Event) []string {
		registryMu.RLock()
		defer registryMu.RUnlock()

		return typeToNames[fmt.Sprintf("%T", event)]
	}
)

// eventPtr constrains PT to a pointer to T that implements [Event], letting
// [RegisterEvent] recover T from PT and mint new(T) itself, rather than
// storing and replaying the single value the caller passed in.
type eventPtr[T any] interface {
	*T
	Event
}

// RegisterEvent registers the concrete event type T — inferred from the
// pointer passed in, whose value is otherwise discarded — under its default
// [Event.EventType] name. Each later [NewEventByName] call for that name
// returns a fresh new(T), so unlike a hand-written closure over a single
// instance, concurrent or repeated decodes never alias the same value. It
// panics if an event is already registered under that name.
//
// Example Usage:
//
//	RegisterEvent(&OrderCreated{})
func RegisterEvent[T any, PT eventPtr[T]](_ PT) {
	RegisterEventByType(func() Event {
		return PT(new(T))
	})
}

// registerEventNameDefault is the internal implementation for registering an event under a name.
//
// It validates the factory function, ensures uniqueness, and stores the factory.
func registerEventNameDefault(name string, fn func() Event) {
	if fn == nil {
		panic("cannot register nil factory")
	}

	if name == "" {
		panic("cannot register factory for empty name")
	}

	registryMu.Lock()
	defer registryMu.Unlock()

	if _, exists := registry[name]; exists {
		panic(fmt.Sprintf("event already registered: %s", name))
	}

	ev := fn()
	if ev == nil {
		panic(fmt.Sprintf("factory returned nil for event: %s", name))
	}

	registry[name] = fn

	key := fmt.Sprintf("%T", ev)
	typeToNames[key] = append(typeToNames[key], name)
}

// newEventByNameDefault is the internal implementation of NewEventByName.
//
// It retrieves the factory for the given name and returns a new instance.
// Returns an error if the event is not registered or the factory returns nil.
func newEventByNameDefault(name string) (Event, error) {
	registryMu.RLock()
	factory, ok := registry[name]
	registryMu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrEventNotRegistered, name)
	}
	ev := factory()
	if ev == nil {
		return nil, fmt.Errorf("factory returned nil for event: %s", name)
	}
	return ev, nil
}
