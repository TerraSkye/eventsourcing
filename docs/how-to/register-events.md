# How to register events for serialization

Register event types so the event store can deserialize them by name when reading from a persistent backend (KurrentDB, file store).

## Why registration is needed

Persistent event stores serialize events to JSON and store them alongside their type name (`EventType()`). When loading events back, the store needs to know which Go struct to unmarshal into. The event registry provides this mapping.

The in-memory store does not need registration because events are never serialized.

## Register an event type

Call `RegisterEvent` with a zero-value instance at startup (e.g., in an `init()` function or `main()`):

```go
import "github.com/terraskye/eventsourcing"

func init() {
    eventsourcing.RegisterEvent(&events.TaskCreated{})
    eventsourcing.RegisterEvent(&events.TaskCompleted{})
    eventsourcing.RegisterEvent(&events.TaskArchived{})
}
```

The registry calls `EventType()` on the instance to determine the lookup key.

## Register with a factory function

Use `RegisterEventByType` when you need to control instantiation:

```go
eventsourcing.RegisterEventByType(func() eventsourcing.Event {
    return &events.TaskCreated{}
})
```

## Register under a custom name

Use `RegisterEventByName` when the stored name differs from `EventType()` — for example, when renaming an event type while preserving backward compatibility:

```go
// Old events in the store are stored as "task.created"
eventsourcing.RegisterEventByName("task.created", func() eventsourcing.Event {
    return &events.TaskCreated{}
})

// New events use the default name from EventType()
eventsourcing.RegisterEvent(&events.TaskCreated{})
```

A single Go type can be registered under multiple names.

## Create an event instance by name

The registry is also used internally by the store. You can use it directly if needed:

```go
ev, err := eventsourcing.NewEventByName("TaskCreated")
if err != nil {
    // ErrEventNotRegistered
}
taskCreated := ev.(*events.TaskCreated)
```

## Panics

`RegisterEvent`, `RegisterEventByType`, and `RegisterEventByName` **panic** if:

- The factory function is nil.
- The factory returns nil.
- The event name is already registered.

Register all events once at startup; do not register them conditionally or repeatedly.
