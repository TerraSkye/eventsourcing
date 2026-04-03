# Event registry

The event registry maps event type names to factory functions. Persistent event stores use it to deserialize events from storage.

## Functions

### RegisterEvent

```go
var RegisterEvent func(event Event)
```

Registers an event type using its `EventType()` name as the key.

```go
eventsourcing.RegisterEvent(&events.TaskCreated{})
```

### RegisterEventByType

```go
var RegisterEventByType func(fn func() Event)
```

Registers using a factory function. Equivalent to `RegisterEvent` but with explicit factory control:

```go
eventsourcing.RegisterEventByType(func() eventsourcing.Event {
    return &events.TaskCreated{}
})
```

### RegisterEventByName

```go
var RegisterEventByName func(name string, fn func() Event)
```

Registers under a custom name, independent of `EventType()`. Useful for migration when stored event names differ from current type names.

```go
// stored as "task.created" historically
eventsourcing.RegisterEventByName("task.created", func() eventsourcing.Event {
    return &events.TaskCreated{}
})
```

A single Go type can be registered under multiple names.

### NewEventByName

```go
var NewEventByName func(name string) (Event, error)
```

Creates a new instance of a registered event by name. Returns `ErrEventNotRegistered` if not found.

```go
ev, err := eventsourcing.NewEventByName("TaskCreated")
if err != nil {
    // handle ErrEventNotRegistered
}
created := ev.(*events.TaskCreated)
```

### EventNamesFor

```go
var EventNamesFor func(event Event) []string
```

Returns all names under which the given event type is registered.

```go
names := eventsourcing.EventNamesFor(&events.TaskCreated{})
// ["TaskCreated"] or ["TaskCreated", "task.created"] if registered under both
```

Used internally by `EventGroupProcessor.StreamFilter()`.

---

## Behaviour notes

- All registration functions are **package-level variables**. They can be replaced in tests to inject alternative implementations.
- Registration **panics** on nil factory, nil result, empty name, or duplicate name.
- The registry is protected by a `sync.RWMutex` and is safe for concurrent reads.
- Register all events at startup (typically in `init()` functions or `main()`).
