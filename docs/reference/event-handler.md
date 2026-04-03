# EventHandler & EventGroupProcessor

## OnEvent

```go
func OnEvent[T Event](fn func(ctx context.Context, ev T) error) EventHandler
```

Creates a strongly-typed `EventHandler` for a specific event type `T`. When dispatched an event of the wrong type, it returns `*ErrSkippedEvent` (not treated as an error by the bus).

```go
handler := eventsourcing.OnEvent(func(ctx context.Context, ev *events.TaskCreated) error {
    fmt.Println("task created:", ev.TaskID)
    return nil
})
```

---

## NewEventHandlerFunc

```go
func NewEventHandlerFunc(fn func(ctx context.Context, event Event) error) EventHandler
```

Creates an untyped `EventHandler` from a plain function. Receives all events without type filtering. Prefer `OnEvent` for type safety.

---

## NewEventGroupProcessor

```go
func NewEventGroupProcessor(handlers ...EventHandler) *EventGroupProcessor
```

Groups multiple typed `EventHandler` values and routes incoming events to the correct handler by type name.

**Panics** if:
- A handler does not implement `EventName() string` (i.e., was not created with `OnEvent`).
- Two handlers are registered for the same event type.

```go
processor := eventsourcing.NewEventGroupProcessor(
    eventsourcing.OnEvent(projector.OnTaskCreated),
    eventsourcing.OnEvent(projector.OnTaskCompleted),
)
```

### EventGroupProcessor methods

```go
func (p *EventGroupProcessor) Handle(ctx context.Context, ev Event) error
```

Routes the event to the matching handler. Returns `*ErrSkippedEvent` if no handler is registered for this event type.

```go
func (p *EventGroupProcessor) StreamFilter() []string
```

Returns a sorted slice of all event type names handled by this group. Used by the KurrentDB event bus to subscribe only to relevant streams.

---

## EventHandler interface

```go
type EventHandler interface {
    Handle(ctx context.Context, event Event) error
}
```

Implement this interface directly when you need custom dispatch logic. For most use cases, `OnEvent` + `NewEventGroupProcessor` is sufficient.

---

## Context values in event handlers

When an event is dispatched via the event bus, the context is enriched with envelope fields:

```go
func (p *Projector) OnTaskCreated(ctx context.Context, e *events.TaskCreated) error {
    streamID := eventsourcing.StreamIDFromContext(ctx)
    version  := eventsourcing.VersionFromContext(ctx)
    metadata := eventsourcing.MetadataFromContext(ctx)
    // ...
}
```

See [Context helpers](./context.md) for the full list.
