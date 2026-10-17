# Part 5: Background Processing

Auto-archive tasks 30 days after completion. You will learn how to **trigger commands from events** with an automation that works off a todo list.

**Time**: ~20 minutes
**Prerequisite**: [Part 4](./04-projections.md) complete.

## Projections vs automations

| | Projection | Automation |
|---|---|---|
| Reads | Events | A todo list (a projection) |
| Produces new events | No | Yes (via commands) |
| Writes read models | Yes | No |
| Example | `Task list` | `Archive completed tasks` |

An **automation** (in other libraries a processor, saga or policy) does work and records the outcome with a command. In Event Modeling it never reacts to events directly. A projection lists the pending work, the **todo list**, and the automation asks it what to do:

```
TaskCompleted ──► tasks to archive (todo list) ──► archive-tasks automation
      ▲                                                    │
      └──────────── TaskArchived ◄──── ArchiveTask ◄───────┘
```

The event the command records takes the item off the todo list again, which closes the loop. Because the todo list is rebuilt from the log, pending work survives a restart, and finished work never comes back.

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
func (c ArchiveTask) CommandType() string { return "ArchiveTask" }

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

## Step 2: Build the todo list

The todo list is an ordinary projection. It lists every completed task that is not archived yet, with the moment it becomes due:

```go
// slices/taskstoarchive/todo_list.go
package taskstoarchive

import (
    "context"
    "sort"
    "sync"
    "time"

    "github.com/google/uuid"
    "github.com/terraskye/eventsourcing"
    "github.com/terraskye/eventsourcing/projection"
    "task-management/events"
)

type item struct {
    due          time.Time // completed at + delay
    claimedUntil time.Time // leased to the automation until then
}

// TasksToArchive is the todo list: written by the projection, claimed by
// the automation.
type TasksToArchive struct {
    mu    sync.Mutex
    delay time.Duration
    items map[uuid.UUID]*item
}

func NewTasksToArchive(delay time.Duration) *TasksToArchive {
    return &TasksToArchive{delay: delay, items: map[uuid.UUID]*item{}}
}

func NewProjection(list *TasksToArchive) projection.Definition {
    return projection.Definition{
        Name:       "tasks-to-archive",
        Handler:    eventsourcing.NewEventHandlerFunc(list.handle),
        EventTypes: []string{"TaskCompleted", "TaskArchived"},
    }
}

func (l *TasksToArchive) handle(ctx context.Context, event eventsourcing.Event) error {
    l.mu.Lock()
    defer l.mu.Unlock()
    switch e := event.(type) {
    case *events.TaskCompleted:
        l.items[e.TaskID] = &item{due: e.CompletedAt.Add(l.delay)}
    case *events.TaskArchived:
        delete(l.items, e.TaskID)
    }
    return nil
}

// Claim leases up to max due tasks, so that a second claim skips them until
// the lease runs out.
func (l *TasksToArchive) Claim(ctx context.Context, max int, lease time.Duration) ([]uuid.UUID, error) {
    l.mu.Lock()
    defer l.mu.Unlock()
    now := time.Now()
    due := []uuid.UUID{}
    for id, it := range l.items {
        if !it.due.After(now) && !it.claimedUntil.After(now) {
            due = append(due, id)
        }
    }
    sort.Slice(due, func(i, j int) bool { return due[i].String() < due[j].String() })
    if len(due) > max {
        due = due[:max]
    }
    for _, id := range due {
        l.items[id].claimedUntil = now.Add(lease)
    }
    return due, nil
}
```

`Claim` leases the items it returns. If the automation crashes halfway, the lease runs out and the items are claimed again; if the work succeeds, `TaskArchived` removes them first. With the todo list in Postgres, the same claim is one `UPDATE ... FOR UPDATE SKIP LOCKED`, so several instances can share the work.

## Step 3: Write the automation

The automation is a `Definition`: the todo list to wait for, how to claim, and the work for one item:

```go
// automations/archivetasks/automation.go
package archivetasks

import (
    "context"

    "github.com/google/uuid"
    "github.com/terraskye/eventsourcing"
    "github.com/terraskye/eventsourcing/automation"
    "task-management/slices/archivetask"
    "task-management/slices/taskstoarchive"
)

func NewAutomation(
    list *taskstoarchive.TasksToArchive,
    todoList automation.Dependency,
    archive eventsourcing.CommandHandler[archivetask.ArchiveTask],
) automation.Definition[uuid.UUID] {
    return automation.Definition[uuid.UUID]{
        Name:     "archive-tasks",
        TodoList: todoList,
        Claim:    list.Claim,
        Work: func(ctx context.Context, taskID uuid.UUID) error {
            _, err := archive(ctx, archivetask.ArchiveTask{TaskID: taskID})
            return err
        },
    }
}
```

The automation only claims while its todo list is **live**. While the projection catches up after a start, it still lists tasks whose `TaskArchived` it has not read yet; claiming then would archive them twice. The runner from Part 4 tells the automation when it is live, and nudges it whenever the todo list changes.

## Step 4: Wire it up

```go
// In main.go, after the task list from Part 4:
archiveList := taskstoarchive.NewTasksToArchive(5 * time.Second) // 30 * 24 * time.Hour in production
archiveTodo := projection.NewRunner(taskstoarchive.NewProjection(archiveList), store, memory.NewProjectionStore())

archiveTasks := automation.New(archivetasks.NewAutomation(
    archiveList,
    archiveTodo,
    archivetask.NewHandler(store),
))

go func() {
    if err := archiveTodo.Run(ctx); err != nil && ctx.Err() == nil {
        log.Fatal(err)
    }
}()
go func() {
    if err := archiveTasks.Run(ctx); err != nil && ctx.Err() == nil {
        log.Fatal(err)
    }
}()
```

A due task waits for the next check, every five seconds by default (`Interval` in the definition). `archiveTasks.Status()` reports what the automation is doing: waiting for its todo list, idle, working or stalled, with counts of processed and failed items.

## Step 5: Test it

```bash
# Create and complete a task
TASK_ID=$(curl -s -X POST http://localhost:8080/api/v1/tasks \
  -H "Content-Type: application/json" \
  -d '{"title": "Auto-archive me"}' | jq -r '.task_id')

curl -X POST "http://localhost:8080/api/v1/tasks/$TASK_ID/complete"

# Wait about ten seconds, then check the event store
# The task stream should now have: TaskCreated → TaskCompleted → TaskArchived
```

Restart the server before the ten seconds are up: the todo list is rebuilt from the log, still lists the task, and the automation archives it after the restart.

## Idempotent commands

Notice that `decide` in `ArchiveTask` returns `nil, nil` (no events, no error) when the task is already archived. This makes the command **idempotent** — safe to retry without side effects. Work is delivered at least once, since an item whose lease runs out is claimed again, so always make the commands an automation sends idempotent.

## What you've learned

Across all five parts you have:

- Built commands with `NewCommandHandler`, `evolve`, and `decide`
- Built live and cached read models with queries
- Kept a projection up to date with the projection runner
- Written an automation that works off a todo list and issues commands

These are the building blocks of every event-sourced system. From here:

- See [Explanation: Event Sourcing Concepts](../explanation/event-sourcing.md) to deepen your understanding
- See [How-to: Handle Concurrency](../how-to/handle-concurrency.md) for production-ready optimistic locking
- See [How-to: Use KurrentDB](../how-to/use-kurrentdb.md) to replace in-memory storage
