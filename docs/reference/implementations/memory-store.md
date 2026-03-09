# memory.EventStore

An in-memory implementation of `EventStore`. Suitable for testing and prototyping. **Data is lost when the process exits.**

## Package

```
github.com/terraskye/eventsourcing/eventstore/memory
```

## Constructor

```go
func NewMemoryStore(buffer int64) *MemoryStore
```

| Parameter | Description |
|---|---|
| `buffer` | Size of the internal `Events()` channel buffer. |

```go
store := memory.NewMemoryStore(100)
defer store.Close()
```

## Events channel

```go
func (m *MemoryStore) Events() <-chan *eventsourcing.Envelope
```

Returns a channel that receives every saved event. Use this to feed events into the event bus:

```go
go func() {
    for env := range store.Events() {
        bus.Dispatch(env)
    }
}()
```

## Behaviour

- All events are stored in a `map[string][]*Envelope` (per stream) and a global `[]*Envelope` slice.
- `LoadFromAll` uses the global slice; `version` is treated as a slice offset.
- `LoadStream` requires the stream to exist (`StreamExists` semantics).
- `LoadStreamFrom` with `Any{}` or `Revision` starts from the given offset.
- Thread-safe via `sync.RWMutex`.
- `Close` clears all stored events and closes the `Events()` channel — do not use the store after calling `Close`.

## Limitations

- No durability — data lost on process exit.
- No event registration required (events are not serialized).
- `LoadFromAll` positions are ordinal offsets, not global sequence numbers.
