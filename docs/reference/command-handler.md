# CommandHandler

## Type

```go
type CommandHandler[C Command] func(ctx context.Context, command C) (AppendResult, error)
```

A `CommandHandler[C]` is a function that handles a specific command type `C`. It:

1. Loads the event stream for the aggregate.
2. Rebuilds state via `Evolver[T]`.
3. Applies business rules via `Decider[T, C]`.
4. Persists the resulting events.
5. Returns `AppendResult` or an error.

---

## NewCommandHandler

```go
func NewCommandHandler[T any, C Command](
    store     EventStore,
    initial   InitialState[T],
    evolve    Evolver[T],
    decide    Decider[T, C],
    opts      ...CommandHandlerOption,
) CommandHandler[C]
```

Creates a `CommandHandler[C]` from the three functional pieces.

### Parameters

| Parameter | Type | Description |
|---|---|---|
| `store` | `EventStore` | Loads and saves events. |
| `initial` | `InitialState[T]` | Factory for the zero state. Called once per command execution. |
| `evolve` | `Evolver[T]` | Rebuilds state from a single event. Called for each event in the stream. |
| `decide` | `Decider[T, C]` | Returns events to persist, or an error to reject the command. |
| `opts` | `...CommandHandlerOption` | Optional configuration (see below). |

### Type parameters

| Parameter | Constraint | Description |
|---|---|---|
| `T` | `any` | Aggregate state type. |
| `C` | `Command` | Command type. |

---

## Functional types

### InitialState[T]

```go
type InitialState[T any] func() T
```

Returns a new instance of the initial aggregate state. Called before loading events to avoid sharing state between concurrent command executions.

```go
var initialState = func() taskState { return taskState{} }
```

### Evolver[T]

```go
type Evolver[T any] func(currentState T, envelope *Envelope) T
```

A pure function that applies one event to the current state and returns the new state. Must not have side effects.

```go
func evolve(state taskState, env *eventsourcing.Envelope) taskState {
    switch env.Event.(type) {
    case *events.TaskCreated:
        return taskState{Exists: true}
    }
    return state
}
```

### Decider[T, C]

```go
type Decider[T any, C Command] func(state T, cmd C) ([]Event, error)
```

Enforces business rules. Returns:
- A non-empty slice of events to persist if the command is accepted.
- An empty slice (and nil error) for no-op commands (e.g. already in the desired state).
- An error to reject the command — wrapped in `ErrBusinessRuleViolation`.

```go
func decide(state taskState, cmd CreateTask) ([]eventsourcing.Event, error) {
    if state.Exists {
        return nil, fmt.Errorf("task already exists")
    }
    return []eventsourcing.Event{&events.TaskCreated{...}}, nil
}
```

---

## Options

### WithStreamState

```go
func WithStreamState(rev StreamState) CommandHandlerOption
```

Sets the concurrency check for saving events. Default: `Any{}`. See [Stream states](./stream-states.md).

### WithRetryStrategy

```go
func WithRetryStrategy(strategy backoff.BackOff) CommandHandlerOption
```

Retries the entire load-evolve-decide-save cycle on `StreamRevisionConflictError`. Default: no retries.

```go
import "github.com/cenkalti/backoff/v4"

eventsourcing.WithRetryStrategy(
    backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 5),
)
```

### WithMetadataExtractor

```go
func WithMetadataExtractor(fn func(ctx context.Context) map[string]any) CommandHandlerOption
```

Adds key-value pairs to every event envelope's `Metadata`. Multiple extractors are merged. See [How to add metadata](../how-to/add-event-metadata.md).

### WithStreamNamer

```go
func WithStreamNamer(namer StreamNamer) CommandHandlerOption
```

Overrides the stream name for this handler. Default: `cmd.AggregateID()`. See [How to customise stream names](../how-to/custom-stream-names.md).

---

## StreamNamer

```go
type StreamNamer func(ctx context.Context, cmd Command) string

var DefaultStreamNamer StreamNamer // overrideable global default
```

---

## Error behaviour

| Situation | Error type |
|---|---|
| `decide` returns an error | Wrapped in `*ErrBusinessRuleViolation` |
| Stream revision mismatch | `*StreamRevisionConflictError` (retried if strategy set) |
| Store load failure | `error` with context |
| Store save failure | `error` with context |
