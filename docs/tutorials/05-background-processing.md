# Part 5: Background Processing

Auto-archive tasks 30 days after completion. You will learn how to **trigger commands from events** using a processor.

**Time**: ~15 minutes
**Prerequisite**: [Part 4](./04-projections.md) complete.

## Processors vs projectors

| | Projector | Processor |
|---|---|---|
| Listens to events | Yes | Yes |
| Produces new events | No | Yes (via commands) |
| Mutates read models | Yes | No |
| Example | `TaskList cache` | `Archive completed tasks` |

A **processor** (also called a saga or policy) reacts to domain events by issuing new commands. It closes the loop: events cause new state changes, which cause new events.

## Step 1: Add an ArchiveTask command and event

```go
// events/task_archived.go
package events

import (
    "time"
    "github.com/google/uuid"
)

type TaskArchived struct {
    TaskID     uuid.UUID `json:"task_id"`
    ArchivedAt time.Time `json:"archived_at"`
}

func (e *TaskArchived) AggregateID() string { return e.TaskID.String() }
func (e *TaskArchived) EventType() string   { return "TaskArchived" }
```

```go
// slices/archivetask/command.go
package archivetask

import (
    "fmt"
    "time"

    "github.com/google/uuid"
    "github.com/terraskye/eventsourcing"
    "task-management/events"
)

type ArchiveTask struct {
    TaskID uuid.UUID
}

func (c ArchiveTask) AggregateID() string { return c.TaskID.String() }

type taskState struct {
    Completed bool
    Archived  bool
}

var initialState = func() taskState { return taskState{} }

func evolve(state taskState, envelope *eventsourcing.Envelope) taskState {
    switch envelope.Event.(type) {
    case *events.TaskCompleted:
        return taskState{Completed: true}
    case *events.TaskArchived:
        return taskState{Completed: true, Archived: true}
    }
    return state
}

func decide(state taskState, cmd ArchiveTask) ([]eventsourcing.Event, error) {
    if !state.Completed {
        return nil, fmt.Errorf("task %s is not completed", cmd.TaskID)
    }
    if state.Archived {
        return nil, nil // idempotent — already archived, no events
    }

    return []eventsourcing.Event{
        &events.TaskArchived{
            TaskID:     cmd.TaskID,
            ArchivedAt: time.Now(),
        },
    }, nil
}

func NewHandler(store eventsourcing.EventStore) eventsourcing.CommandHandler[ArchiveTask] {
    return eventsourcing.NewCommandHandler(store, initialState, evolve, decide)
}
```

## Step 2: Write the processor

```go
// processors/archivetasks/processor.go
package archivetasks

import (
    "context"
    "log"
    "time"

    "github.com/terraskye/eventsourcing"
    "task-management/events"
    "task-management/slices/archivetask"
)

type Processor struct {
    handler eventsourcing.CommandHandler[archivetask.ArchiveTask]
    delay   time.Duration // configurable — use 30*24*time.Hour in production
}

func NewProcessor(handler eventsourcing.CommandHandler[archivetask.ArchiveTask], delay time.Duration) *Processor {
    return &Processor{handler: handler, delay: delay}
}

// OnTaskCompleted is called when a task is completed.
// It schedules the archive command after the configured delay.
func (p *Processor) OnTaskCompleted(ctx context.Context, e *events.TaskCompleted) error {
    go func() {
        select {
        case <-time.After(p.delay):
            cmd := archivetask.ArchiveTask{TaskID: e.TaskID}
            if _, err := p.handler(context.Background(), cmd); err != nil {
                log.Printf("archive task %s: %v", e.TaskID, err)
            }
        case <-ctx.Done():
            // Subscription cancelled; skip
        }
    }()

    return nil
}

// EventHandlers returns the event group processor to register on the bus.
func (p *Processor) EventHandlers() *eventsourcing.EventGroupProcessor {
    return eventsourcing.NewEventGroupProcessor(
        eventsourcing.OnEvent(p.OnTaskCompleted),
    )
}
```

> **Production note**: Using `time.After` inside a goroutine is acceptable for tutorials, but in production you should use a durable job queue (e.g. Temporal, Asynq) so scheduled work survives restarts.

## Step 3: Wire it up

```go
// In main.go, after creating the bus:
archiveHandler := archivetask.NewHandler(store)
archiveProcessor := archivetasks.NewProcessor(archiveHandler, 5*time.Second) // 5s for testing

bus.Subscribe(context.Background(), "archive-processor", archiveProcessor.EventHandlers())
```

## Step 4: Test it

```bash
# Create and complete a task
TASK_ID=$(curl -s -X POST http://localhost:8080/api/v1/tasks \
  -H "Content-Type: application/json" \
  -d '{"title": "Auto-archive me"}' | jq -r '.task_id')

curl -X POST "http://localhost:8080/api/v1/tasks/$TASK_ID/complete"

# Wait 6 seconds, then check the event store
# The task stream should now have: TaskCreated → TaskCompleted → TaskArchived
```

## Idempotent commands

Notice that `decide` in `ArchiveTask` returns `nil, nil` (no events, no error) when the task is already archived. This makes the command **idempotent** — safe to retry without side effects. Always make processor-driven commands idempotent, as they may be retried on failure.

## What you've learned

Across all five parts you have:

- Built commands with `NewCommandHandler`, `evolve`, and `decide`
- Built live and cached read models with queries
- Used the event bus to keep projections up to date
- Written a processor that issues commands in response to events

These are the building blocks of every event-sourced system. From here:

- See [Explanation: Event Sourcing Concepts](../explanation/event-sourcing.md) to deepen your understanding
- See [How-to: Handle Concurrency](../how-to/handle-concurrency.md) for production-ready optimistic locking
- See [How-to: Use KurrentDB](../how-to/use-kurrentdb.md) to replace in-memory storage
