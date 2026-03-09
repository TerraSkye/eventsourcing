# memory.EventBus

An in-memory implementation of `EventBus`. Each subscriber gets its own buffered channel and goroutine. Suitable for testing and single-process applications.

## Package

```
github.com/terraskye/eventsourcing/eventbus/memory
```

## Constructor

```go
func NewEventBus(bufferSize int) *EventBus
```

| Parameter | Description |
|---|---|
| `bufferSize` | Per-subscriber channel buffer size. |

```go
bus := memory.NewEventBus(100)
defer bus.Close()
```

## Dispatch

```go
func (b *EventBus) Dispatch(ev *eventsourcing.Envelope)
```

Sends the event to all matching subscribers. Events are matched by `ev.Event.EventType()` against each subscriber's filter. If no filter is set, the subscriber receives all events.

This is **not** part of the `EventBus` interface — it is specific to the in-memory implementation. Call it from the event forwarding goroutine:

```go
go func() {
    for env := range store.Events() {
        bus.Dispatch(env)
    }
}()
```

## WithFilterEvents

```go
func WithFilterEvents(filteredEvents []string) eventsourcing.SubscriberOption
```

Limits a subscriber to specific event types:

```go
bus.Subscribe(ctx, "my-handler", handler,
    memory.WithFilterEvents([]string{"TaskCreated", "TaskCompleted"}),
)
```

Pass `nil` or an empty slice to receive all events.

## Behaviour

- Each subscriber has an isolated goroutine processing its buffered channel.
- If the subscriber buffer is full, `Dispatch` blocks momentarily (select with no default for `s.events <- ev`).
- Errors from handlers are sent to `Errors()`. If the error channel is full, the error is dropped.
- Cancelling the subscription's context removes the subscriber and closes its channel.
- `Close` shuts down all subscribers and waits for goroutines to exit.
