# The aggregate variant

This library supports two ways to organise command handler state and `evolve`. Understanding both — and when each breaks down — helps you make the right call early.

## The default: per-slice state

In the vertical slice approach, each command handler owns its own `state` struct and `evolve` function:

```
tasks/
├── events/
└── slices/
    ├── createtask/
    │   └── command.go    ← state, evolve, decide all local
    └── completetask/
        └── command.go    ← its own state, evolve, decide
```

The `createtask` handler only tracks `Exists`. The `completetask` handler tracks `Exists` and `Completed`. They look similar, but they are **independent copies** — each free to diverge.

## The aggregate variant

You can instead move state and `evolve` into a shared `aggregate` folder that all command handlers import:

```
tasks/
├── aggregate/
│   └── task.go          ← shared TaskState + Evolve
├── events/
└── slices/
    ├── createtask/
    │   └── command.go    ← imports aggregate.TaskState, aggregate.Evolve
    └── completetask/
        └── command.go    ← same import
```

```go
// tasks/aggregate/task.go
package aggregate

import "github.com/terraskye/eventsourcing"
import "task-management/tasks/events"

type TaskState struct {
    Exists    bool
    Completed bool
    Archived  bool
}

func InitialState() TaskState { return TaskState{} }

func Evolve(state TaskState, envelope *eventsourcing.Envelope) TaskState {
    switch envelope.Event.(type) {
    case *events.TaskCreated:
        return TaskState{Exists: true}
    case *events.TaskCompleted:
        return TaskState{Exists: true, Completed: true}
    case *events.TaskArchived:
        return TaskState{Exists: true, Completed: state.Completed, Archived: true}
    }
    return state
}
```

Each command handler then passes `aggregate.InitialState` and `aggregate.Evolve` to `NewCommandHandler`, and only `decide` stays local:

```go
// tasks/slices/completetask/command.go
func NewHandler(store eventsourcing.EventStore) eventsourcing.CommandHandler[CompleteTask] {
    return eventsourcing.NewCommandHandler(store, aggregate.InitialState, aggregate.Evolve, decide)
}
```

This is valid Go and the framework supports it fully.

## Why the vertical approach is usually better

The aggregate variant looks clean when all handlers happen to need the same fields. In practice, subtle differences appear:

- `ArchiveTask` needs to know the `AssigneeID` to notify them — but `CreateTask` does not.
- `TransferTask` needs the current owner — but no other handler does.
- A new `PauseTask` command needs a `PausedAt` timestamp that would be noise everywhere else.

Each time, you face the same choice: add the field to `TaskState` for all handlers, or split the aggregate. Both options grow in difficulty with each edge case. The shared `evolve` accumulates logic that only some handlers need, and `TaskState` becomes a union of every handler's requirements rather than the minimal state for any one.

With the per-slice approach, each handler carries only what it needs. Adding a field to `completetask/command.go` does not touch `createtask`. **Copying is cheaper than managing divergence.**

## When the aggregate variant still makes sense

- A tightly related group of handlers (e.g., a multi-step workflow where every step needs the same invariants) where the state genuinely does not diverge.
- A team convention where a single place for event-to-state mapping is preferable for discoverability.

Even then, watch for the first subtle difference — that is the signal to split back into per-slice state.

## Rule of thumb

Start with per-slice state. If you find yourself copying a large, stable `evolve` across many handlers with no variation after several months, the aggregate variant is reasonable. Do not start there.
