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
}
```

A `Command` represents intent to change state. It targets a specific aggregate identified by `AggregateID()`.

Commands are named in the present tense (`CreateTask`, not `TaskCreated`). They can be rejected by business rules in `decide`.

**Example:**

```go
type CreateTask struct {
    TaskID uuid.UUID
    Title  string
}

func (c CreateTask) AggregateID() string { return c.TaskID.String() }
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
    LoadFromAll(ctx context.Context, version StreamState) (*Iterator[*Envelope], error)
    Close() error
}
```

Append-only store for event envelopes. See [Stream states](./stream-states.md) for the `revision` / `version` parameter.

- `Save` — appends events with an optional concurrency check.
- `LoadStream` — loads all events for a stream (requires stream to exist).
- `LoadStreamFrom` — loads events from a given revision.
- `LoadFromAll` — loads all events across all streams from a given position.
- `Close` — releases resources.

---

## EventBus

```go
type EventBus interface {
    Subscribe(ctx context.Context, name string, handler EventHandler, options ...SubscriberOption) error
    Errors() <-chan error
    Close() error
}
```

Distributes events to registered handlers.

- `Subscribe` — registers a named handler. The context controls the handler's lifetime; when cancelled, the subscription is removed.
- `Errors` — channel for asynchronous handler errors.
- `Close` — shuts down all subscriptions and waits for in-flight events to finish.

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
