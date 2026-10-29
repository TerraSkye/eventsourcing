# Core interfaces

## Event

```go
type Event interface {
    AggregateID() string
    EventType() string
}
```

An `Event` represents an immutable fact that has occurred. Implement this interface on every domain event struct.

- `AggregateID()` — returns the ID of the aggregate this event belongs to. Used to route the event to its stream in the event store.
- `EventType()` — returns a stable string name used for serialization/deserialization. Must be unique across all registered events.

**Example:**

```go
type TaskCreated struct {
    TaskID    uuid.UUID
    Title     string
    CreatedAt time.Time
}

func (e *TaskCreated) AggregateID() string { return e.TaskID.String() }
func (e *TaskCreated) EventType() string   { return "TaskCreated" }
```

---

## Command

```go
type Command interface {
    AggregateID() string
    CommandType() string
}
```

A `Command` represents intent to change state. It targets a specific aggregate identified by `AggregateID()`.

- `AggregateID()` — returns the ID of the aggregate this command targets. Used to load that aggregate's state and to pick the `CommandBus` shard.
- `CommandType()` — returns a stable string name used as the `CommandBus` registration and routing key. Must be unique across all registered commands, and must not vary between instances: `Register` reads it off the zero value.

Commands are named in the present tense (`CreateTask`, not `TaskCreated`). They can be rejected by business rules in `decide`.

**Example:**

```go
type CreateTask struct {
    TaskID uuid.UUID
    Title  string
}

func (c CreateTask) AggregateID() string { return c.TaskID.String() }
func (c CreateTask) CommandType() string { return "CreateTask" }
```

---

## Query

```go
type Query interface {
    ID() []byte
}
```

A `Query` requests data without changing state. `ID()` returns a byte key used for routing in the `QueryBus`.

**Example:**

```go
type GetTask struct {
    TaskID uuid.UUID
}

func (q GetTask) ID() []byte { return q.TaskID[:] }
```

---

## EventStore

```go
type EventStore interface {
    Save(ctx context.Context, events []Envelope, revision StreamState) (AppendResult, error)
    LoadStream(ctx context.Context, id string) (*Iterator[*Envelope], error)
    LoadStreamFrom(ctx context.Context, id string, version StreamState) (*Iterator[*Envelope], error)
    Close() error
}
```

Append-only store for event envelopes. See [Stream states](./stream-states.md) for the `revision` / `version` parameter.

- `Save` — appends events with an optional concurrency check. Every envelope must carry its `EventID`; `Save` returns `ErrInvalidEventBatch` otherwise.
- `LoadStream` — loads all events for a stream (requires stream to exist).
- `LoadStreamFrom` — loads events from a given revision.
- `Close` — releases resources.

Reading across all streams is not part of `EventStore`; see [GlobalReader](#globalreader).

---

## GlobalReader

```go
type GlobalReader interface {
    ReadAll(ctx context.Context, after uint64, opts ReadOptions) (ReadResult, error)
    Head(ctx context.Context) (uint64, error)
}
```

Reads the global log — every event across all streams — in strictly ascending `GlobalPosition` order, a page at a time. Implemented by the memory, file, Postgres and KurrentDB stores.

- `ReadAll` — returns the events after position `after` (exclusive; `0` reads from the start), up to `opts.Limit`, optionally filtered by `opts.EventTypes` (exact names) and `opts.StreamPrefixes`. `ReadResult.Next` is the position to pass as `after` next time, and moves past filtered-out events; `ReadResult.AtEnd` reports the read reached the head. It never blocks waiting for new events.
- `Head` — the `GlobalPosition` of the newest event `ReadAll` could return, ignoring filters, or `0` for an empty log.

Stores guarantee that no event becomes visible behind a position already returned, so a reader that checkpoints `Next` never misses an event. `eventsourcingtest.GlobalReaderAcceptanceTest` checks an implementation against the full contract.

`ReadAll` does not block at the head of the log; readers poll. The [projection runner](../how-to/cached-projection.md) is built on `GlobalReader` and handles polling and checkpointing for you.

---

## CorrelationLoader

```go
type CorrelationLoader interface {
    LoadCorrelation(ctx context.Context, id string) (*Iterator[*Envelope], error)
}
```

Loads a correlation chain: every event, across all streams, whose `Envelope.CorrelationID` is `id`, in ascending `GlobalPosition` order. Use it to debug or audit everything one command set off. Implemented by the memory, Postgres and SQLite stores.

An `id` no event carries yields an empty iterator. The empty `id` returns an error wrapping `ErrEmptyCorrelationID`, since every event saved without a correlation ID shares it. `eventsourcingtest.CorrelationLoaderAcceptanceTest` checks an implementation against the contract.

---

## EventHandler

```go
type EventHandler interface {
    Handle(ctx context.Context, event Event) error
}
```

Processes a single event. Use `OnEvent[T]` to create a strongly-typed handler, or `NewEventHandlerFunc` for an untyped one. See [EventHandler & EventGroupProcessor](./event-handler.md).

---

## AppendResult

```go
type AppendResult struct {
    Successful          bool
    StreamID            string
    NextExpectedVersion uint64
}
```

Returned by `EventStore.Save` and `CommandHandler`. `NextExpectedVersion` is the version to use for the next write with optimistic locking.
