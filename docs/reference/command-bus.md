# CommandBus

An in-memory, sharded command dispatcher that serialises commands to an aggregate (same `AggregateID` always goes to the same shard) and executes them asynchronously via worker goroutines.

## When to use

Use `CommandBus` when you need:
- A single dispatch point for multiple command types.
- Serialised execution per aggregate (prevents concurrent writes to the same stream without optimistic locking).
- Panic recovery in handlers.

For simpler cases, call `CommandHandler[C]` directly without a bus.

---

## NewCommandBus

```go
func NewCommandBus(bufferSize int, shardCount int) *CommandBus
```

| Parameter | Description |
|---|---|
| `bufferSize` | Size of the internal buffered channel per shard. |
| `shardCount` | Number of shards (workers). Must be > 0. Commands for the same `AggregateID` always go to the same shard (FNV hash). |

```go
bus := eventsourcing.NewCommandBus(100, 4)
```

---

## Register

```go
func Register[C Command](b *CommandBus, handler CommandHandler[C])
```

Registers a typed handler. **Panics** if a handler is already registered for the same command type.

```go
eventsourcing.Register(bus, createTaskHandler)
eventsourcing.Register(bus, completeTaskHandler)
```

---

## Dispatch

```go
func (b *CommandBus) Dispatch(ctx context.Context, cmd Command) (AppendResult, error)
```

Enqueues the command and blocks until it is processed. Respects context cancellation.

Returns `ErrCommandBusClosed` if `Stop` has been called.

```go
result, err := bus.Dispatch(ctx, CreateTask{...})
```

---

## Stop

```go
func (b *CommandBus) Stop()
```

Stops accepting new commands, closes all internal queues, and waits for all in-flight commands to complete. Call this during graceful shutdown.

```go
defer bus.Stop()
```

---

## Dispatcher interface

```go
type Dispatcher interface {
    Dispatch(ctx context.Context, cmd Command) (AppendResult, error)
}
```

`CommandBus` implements `Dispatcher`. Use this interface in your HTTP handlers to decouple them from the bus implementation.

---

## Sharding behaviour

Commands are routed to a shard using FNV-1a hash of `AggregateID()` modulo `shardCount`. All commands for the same aggregate are processed sequentially within their shard, eliminating optimistic locking conflicts for most workloads.

Set `shardCount` to a value that matches your expected concurrency. A good starting point is the number of CPU cores.
