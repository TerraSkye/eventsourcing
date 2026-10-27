# memory.EventStore

An in-memory implementation of `EventStore`. Suitable for testing and prototyping. **Data is lost when the process exits.**

## Package

```
github.com/terraskye/eventsourcing/memory
```

## Constructor

```go
func NewEventStore() *EventStore
```

```go
store := memory.NewEventStore()
defer store.Close()
```

## Behaviour

- Each event is kept encoded by `eventsourcing.EncodeEvent`, as the persistent stores keep it, in a per-stream map and a global slice. Every read decodes it again, so each read returns its own copy of the event and metadata.
- `ReadAll` and `Head` (`GlobalReader`) read the global slice; `GlobalPosition` is the event's index in it plus one.
- `LoadStream` requires the stream to exist (`StreamExists` semantics).
- `LoadStreamFrom` with `Any{}` or `Revision` starts from the given offset.
- Thread-safe via `sync.RWMutex`.
- `Close` clears all stored events; `Save` returns an error after it.

## Limitations

- No durability — data lost on process exit.
- As on the persistent stores, every event type must be registered (see [Event registry](../event-registry.md)); `Save` rejects an unregistered type with `ErrEventNotRegistered`. Reads return each event as the registry makes it, a pointer such as `*TaskCreated`, even for an event saved as a value.
