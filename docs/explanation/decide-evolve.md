# The decide/evolve pattern

The decide/evolve pattern is the core of every command handler. It separates **state reconstruction** (`evolve`) from **business logic** (`decide`).

## The two functions

```
events in store
      │
      │  evolve(state, event) → state
      ▼
 current state
      │
      │  decide(state, command) → events | error
      ▼
 new events (saved to store)
```

### evolve

```go
type Evolver[T any] func(currentState T, envelope *Envelope) T
```

`evolve` answers: "given that this event happened, what is the new state?"

- Pure function — no side effects, no I/O.
- Called once per stored event, in order.
- Returns the updated state.
- Must handle unknown event types gracefully (return state unchanged).

### decide

```go
type Decider[T any, C Command] func(state T, cmd C) ([]Event, error)
```

`decide` answers: "given the current state, can we apply this command, and what events should result?"

- Pure function — no side effects, no I/O.
- Returns events to persist, or an error to reject the command.
- Returns an empty slice (and nil error) for no-op commands.
- Must not mutate the state argument.

## Why two functions?

The separation ensures that `decide` always works with a complete, consistent view of the aggregate's history. This makes business rules easy to express and test.

It also enables **optimistic concurrency**: if two commands are sent simultaneously, the second one reloads events, re-evolves state (including the first command's events), and re-runs `decide` with the updated state. This guarantees business rules are never evaluated against stale state.

## State is minimal

The aggregate state (`T`) should contain only the fields needed to enforce business rules in `decide`. It is not the same as a read model or a database row.

```go
// Good: minimal state for enforcing task business rules
type taskState struct {
    Exists    bool
    Completed bool
}

// Too much: includes read model data not needed for decisions
type taskState struct {
    Exists      bool
    Completed   bool
    Title       string   // not needed for decisions
    Description string   // not needed for decisions
    CreatedAt   time.Time
}
```

Each command handler has its own state struct. A `CreateTask` handler only needs to know if the task exists. A `CompleteTask` handler needs to know if it exists and whether it's already completed.

## Pattern for unknown events

Because multiple command handlers write to the same stream, an `evolve` function will encounter events it did not produce. Always return state unchanged for unhandled events:

```go
func evolve(state taskState, env *eventsourcing.Envelope) taskState {
    switch env.Event.(type) {
    case *events.TaskCreated:
        return taskState{Exists: true}
    case *events.TaskCompleted:
        return taskState{Exists: true, Completed: true}
    // unknown events: fall through
    }
    return state // unchanged
}
```

## Idempotency

A `decide` function can return no events (empty slice, nil error) to indicate the command had no effect. This is used for idempotent commands — commands that are safe to retry:

```go
func decide(state taskState, cmd ArchiveTask) ([]eventsourcing.Event, error) {
    if state.Archived {
        return nil, nil // already done — no-op
    }
    return []eventsourcing.Event{&events.TaskArchived{...}}, nil
}
```

## Testing

Because both functions are pure, they are trivially testable without any infrastructure:

```go
func TestDecide_RejectDuplicate(t *testing.T) {
    events, err := decide(taskState{Exists: true}, CreateTask{Title: "X"})
    if err == nil {
        t.Fatal("expected rejection")
    }
    if len(events) != 0 {
        t.Fatal("expected no events on rejection")
    }
}

func TestEvolve_TaskCreated(t *testing.T) {
    state := evolve(taskState{}, &eventsourcing.Envelope{
        Event: &events.TaskCreated{},
    })
    if !state.Exists {
        t.Error("expected Exists = true after TaskCreated")
    }
}
```
