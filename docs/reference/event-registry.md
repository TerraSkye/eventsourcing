# Event registry

The event registry maps event type names to factory functions. Persistent event stores use it to deserialize events from storage.

## Functions

### RegisterEvent

```go
func RegisterEvent[T any, PT eventPtr[T]](_ PT)
```

Registers an event type using its `EventType()` name as the key. The pointer
passed in is only used to infer the concrete type `T`; the value itself is
discarded. Each subsequent lookup gets a genuinely new `new(T)` instance —
built at the type level via generics, not via reflection — so decoding two
stored events never lets one overwrite the other's fields.

```go
eventsourcing.RegisterEvent(&events.TaskCreated{})
```

### RegisterEventByType

```go
func RegisterEventByType(fn func() Event)
```

Registers using a factory function. Equivalent to `RegisterEvent` but with explicit factory control:

```go
eventsourcing.RegisterEventByType(func() eventsourcing.Event {
    return &events.TaskCreated{}
})
```

### RegisterEventByName

```go
func RegisterEventByName(name string, fn func() Event)
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
func NewEventByName(name string) (Event, error)
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
func EventNamesFor(event Event) []string
```

Returns all names under which the given event type is registered.

```go
names := eventsourcing.EventNamesFor(&events.TaskCreated{})
// ["TaskCreated"] or ["TaskCreated", "task.created"] if registered under both
```

Used internally by `EventGroupProcessor.StreamFilter()`.

### EncodeEvent / DecodeEvent

```go
func EncodeEvent(event Event) ([]byte, error)
func DecodeEvent(name string, data []byte) (Event, error)
```

The encoding every store in this module persists events in: JSON, through `encoding/json`. A store saves the bytes `EncodeEvent` returns under `event.EventType()`, and reads them back with `DecodeEvent`. A store outside this module that does the same is byte-compatible with these.

`EncodeEvent` returns an error wrapping `ErrEventNotRegistered` unless the event's type is registered under its `EventType()` name, so an event a store could not read back fails at `Save`. `DecodeEvent` returns `ErrEventNotRegistered` for an unknown name.

```go
data, err := eventsourcing.EncodeEvent(&events.TaskCreated{ID: "t1"})
// ...
ev, err := eventsourcing.DecodeEvent("TaskCreated", data)
```

### EncodeMetadata / DecodeMetadata

```go
func EncodeMetadata(metadata map[string]any) ([]byte, error)
func DecodeMetadata(data []byte) (map[string]any, error)
```

The same pair for an `Envelope`'s `Metadata`. A nil map encodes as `{}`; empty data and `null` decode as an empty, non-nil map.

---

## Behaviour notes

- Registration **panics** on nil factory, nil result, empty name, or duplicate name.
- The registry is protected by a `sync.RWMutex` and is safe for concurrent reads.
- Register all events at startup (typically in `init()` functions or `main()`).
