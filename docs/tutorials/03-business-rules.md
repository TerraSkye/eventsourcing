# Part 3: Business Rules

Add a `CompleteTask` command. You will learn how to use **state** to enforce business rules across multiple commands.

**Time**: ~20 minutes
**Prerequisite**: [Part 2](./02-first-query.md) complete.

## Why state matters

Business rules often depend on what has already happened:

- A task cannot be completed if it doesn't exist.
- A task cannot be completed twice.
- A task must be assigned before it can be completed.

The `evolve` function rebuilds this state from the event history. The `decide` function uses it to guard the command.

## Step 1: Add a second event

```go
// events/task_completed.go
package events

import (
    "time"
    "github.com/google/uuid"
)

type TaskCompleted struct {
    TaskID      uuid.UUID `json:"task_id"`
    CompletedBy uuid.UUID `json:"completed_by"`
    CompletedAt time.Time `json:"completed_at"`
}

func (e *TaskCompleted) AggregateID() string { return e.TaskID.String() }
func (e *TaskCompleted) EventType() string   { return "TaskCompleted" }
```

## Step 2: Write the CompleteTask command handler

This handler shares the same stream as `CreateTask` (same `TaskID`), so it sees the full history.

```go
// slices/completetask/command.go
package completetask

import (
    "fmt"
    "time"

    "github.com/google/uuid"
    "github.com/terraskye/eventsourcing"
    "task-management/events"
)

type CompleteTask struct {
    TaskID      uuid.UUID
    CompletedBy uuid.UUID
}

func (c CompleteTask) AggregateID() string { return c.TaskID.String() }

// taskState tracks everything needed to enforce completion rules.
type taskState struct {
    Exists    bool
    Completed bool
}

var initialState = func() taskState { return taskState{} }

// evolve handles events from both CreateTask and CompleteTask handlers —
// they all write to the same stream.
func evolve(state taskState, envelope *eventsourcing.Envelope) taskState {
    switch envelope.Event.(type) {
    case *events.TaskCreated:
        return taskState{Exists: true, Completed: false}
    case *events.TaskCompleted:
        return taskState{Exists: true, Completed: true}
    }
    return state
}

func decide(state taskState, cmd CompleteTask) ([]eventsourcing.Event, error) {
    if !state.Exists {
        return nil, fmt.Errorf("task %s does not exist", cmd.TaskID)
    }
    if state.Completed {
        return nil, fmt.Errorf("task %s is already completed", cmd.TaskID)
    }

    return []eventsourcing.Event{
        &events.TaskCompleted{
            TaskID:      cmd.TaskID,
            CompletedBy: cmd.CompletedBy,
            CompletedAt: time.Now(),
        },
    }, nil
}

func NewHandler(store eventsourcing.EventStore) eventsourcing.CommandHandler[CompleteTask] {
    return eventsourcing.NewCommandHandler(store, initialState, evolve, decide)
}
```

## Step 3: Add the HTTP handler

```go
// slices/completetask/http.go
package completetask

import (
    "net/http"

    "github.com/gin-gonic/gin"
    "github.com/google/uuid"
    "github.com/terraskye/eventsourcing"
)

type HTTPHandler struct {
    handler eventsourcing.CommandHandler[CompleteTask]
}

func NewHTTPHandler(h eventsourcing.CommandHandler[CompleteTask]) *HTTPHandler {
    return &HTTPHandler{handler: h}
}

func (h *HTTPHandler) Handle(c *gin.Context) {
    taskID, err := uuid.Parse(c.Param("taskID"))
    if err != nil {
        c.JSON(http.StatusBadRequest, gin.H{"error": "invalid task ID"})
        return
    }

    cmd := CompleteTask{
        TaskID:      taskID,
        CompletedBy: uuid.New(), // replace with user from auth context
    }

    if _, err := h.handler(c.Request.Context(), cmd); err != nil {
        c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
        return
    }

    c.Status(http.StatusNoContent)
}

func (h *HTTPHandler) RegisterRoutes(g *gin.RouterGroup) {
    g.POST("/:taskID/complete", h.Handle)
}
```

## Step 4: Wire it up and test

Add to `main.go`:

```go
import "task-management/slices/completetask"

completeTaskHandler := completetask.NewHandler(store)
completeTaskHTTP := completetask.NewHTTPHandler(completeTaskHandler)
completeTaskHTTP.RegisterRoutes(tasks)
```

```bash
# Create a task
TASK_ID=$(curl -s -X POST http://localhost:8080/api/v1/tasks \
  -H "Content-Type: application/json" \
  -d '{"title": "Finish tutorial"}' | jq -r '.task_id')

# Complete it
curl -X POST "http://localhost:8080/api/v1/tasks/$TASK_ID/complete"
# → 204 No Content

# Try to complete it again
curl -X POST "http://localhost:8080/api/v1/tasks/$TASK_ID/complete"
# → 400 Bad Request: task already completed

# Try to complete a non-existent task
curl -X POST "http://localhost:8080/api/v1/tasks/00000000-0000-0000-0000-000000000000/complete"
# → 400 Bad Request: task does not exist
```

## How the state is shared

Both `createtask` and `completetask` write to the same stream — the task's UUID. When `completetask` loads events, it sees:

```
Stream: <task-uuid>
  Version 1: TaskCreated{...}
  Version 2: TaskCompleted{...}   ← after completion
```

The `evolve` function in `completetask` handles **both** event types so it can correctly reconstruct the full state from any stream position.

## Key insights

**State is ephemeral** — it's always rebuilt from events. Never persist `taskState` to a database; always replay events.

**Each command handler has its own state struct** — the state only needs to contain what's required for *this* handler's business rules. `completetask` only needs `Exists` and `Completed`; it doesn't need the task title.

**Decide is a pure function** — given the same state and command, it always produces the same events. This makes it trivially testable:

```go
func TestCompleteTask_AlreadyCompleted(t *testing.T) {
    state := taskState{Exists: true, Completed: true}
    _, err := decide(state, CompleteTask{TaskID: uuid.New()})
    if err == nil {
        t.Fatal("expected error")
    }
}
```

---

Next: [Part 4: Real-time Projections](./04-projections.md) — use the event bus to keep a cached read model up to date.
