# Errors

## Sentinel errors

```go
var ErrStreamNotFound       = errors.New("stream not found")
var ErrStreamExists         = errors.New("stream already exists")
var ErrInvalidEventBatch    = errors.New("invalid event batch")
var ErrHandlerNotFound      = errors.New("handler not registered")
var ErrInvalidRevision      = errors.New("invalid revision")
var ErrHandlerNotRegistered = errors.New("no handler registered for type")
var ErrDuplicateHandler     = errors.New("duplicate handler registered")
var ErrHandlerPanicked      = errors.New("handler panicked when handling command")
var ErrCommandBusClosed     = errors.New("command bus is closed")
var ErrEventNotRegistered   = errors.New("event not registered")
```

Use `errors.Is` to match these:

```go
if errors.Is(err, eventsourcing.ErrStreamNotFound) {
    // stream does not exist
}
```

---

## ErrBusinessRuleViolation

```go
type ErrBusinessRuleViolation struct {
    // unexported cause
}

func NewBusinessRuleViolation(err error) error
```

Wraps the error returned by a `Decider` function. `NewCommandHandler` already wraps whatever error `decide` returns in one of these automatically — so a `decide` function used with it should just return the plain error (e.g. `fmt.Errorf("task already exists")`), not call `NewBusinessRuleViolation` itself; doing so would nest one violation inside another.

The cause is unexported, so construct one with `NewBusinessRuleViolation` rather than a struct literal. Use it when signaling a business rule violation from a hand-rolled `CommandHandler` that doesn't go through `NewCommandHandler`'s decide step — e.g. so an OpenTelemetry middleware can still recognize it as an expected, recoverable rejection via `errors.As`. If `err` is nil, `NewBusinessRuleViolation` returns nil too:

```go
func handleCreateTask(ctx context.Context, cmd CreateTask) (eventsourcing.AppendResult, error) {
    if err := validate(cmd); err != nil {
        return eventsourcing.AppendResult{}, eventsourcing.NewBusinessRuleViolation(err)
    }
    // ... append the resulting events directly ...
}
```

```go
var violation *eventsourcing.ErrBusinessRuleViolation
if errors.As(err, &violation) {
    cause := violation.Unwrap() // the original error
}
```

Implements `Unwrap() error` and `Cause() error`.

---

## StreamRevisionConflictError

```go
type StreamRevisionConflictError struct {
    Stream           string
    ExpectedRevision StreamState
    ActualRevision   StreamState
}
```

Returned by `EventStore.Save` when the stream version does not match the expected revision. Used by `NewCommandHandler` to trigger retries.

```go
var conflict *eventsourcing.StreamRevisionConflictError
if errors.As(err, &conflict) {
    log.Printf("conflict on %s: expected %v got %v",
        conflict.Stream, conflict.ExpectedRevision, conflict.ActualRevision)
}
```

---

## ErrSkippedEvent

```go
type ErrSkippedEvent struct {
    Event Event
}
```

Returned by a typed event handler (created with `OnEvent`) when the event type does not match, and by `EventGroupProcessor.Handle` when no handler is registered for the event type.

This is **not an error condition** — the event bus ignores `ErrSkippedEvent`. It is returned to enable detection when calling `Handle` directly.

```go
err := handler.Handle(ctx, someEvent)
var skipped *eventsourcing.ErrSkippedEvent
if errors.As(err, &skipped) {
    // event type not handled by this processor
}
```
