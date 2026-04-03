# Part 1: Your First Command

Build your first event-sourced feature: **creating a task**. You will learn how commands, events, and the event store work together.

**Time**: ~30 minutes
**Goal**: A working `POST /tasks` endpoint that persists a `TaskCreated` event.

## What happens when a command runs

```
HTTP Request
    │
    ▼
CreateTask (Command)
    │
    ▼
Load events from store → rebuild taskState via evolve()
    │
    ▼
decide(state, command) → []Event or error
    │
    ▼
Save events to store
    │
    ▼
HTTP Response
```

The framework handles loading, evolving state, and saving. You only write `evolve` and `decide`.

## Step 1: Define the event

Events are **immutable facts**. Name them in the past tense — they describe something that already happened.

```go
// events/task_created.go
package events

import (
    "time"
    "github.com/google/uuid"
)

type TaskCreated struct {
    TaskID      uuid.UUID `json:"task_id"`
    Title       string    `json:"title"`
    Description string    `json:"description"`
    CreatedBy   uuid.UUID `json:"created_by"`
    CreatedAt   time.Time `json:"created_at"`
}

func (e *TaskCreated) AggregateID() string { return e.TaskID.String() }
func (e *TaskCreated) EventType() string   { return "TaskCreated" }
```

`AggregateID()` tells the event store which stream this event belongs to. `EventType()` is used for serialization.

## Step 2: Write the command handler

A command handler has three parts: **state**, **evolve**, and **decide**.

```go
// slices/createtask/command.go
package createtask

import (
    "fmt"
    "time"

    "github.com/google/uuid"
    "github.com/terraskye/eventsourcing"
    "task-management/events"
)

// CreateTask is the command — present tense, expresses intent.
type CreateTask struct {
    TaskID      uuid.UUID
    Title       string
    Description string
    CreatedBy   uuid.UUID
}

func (c CreateTask) AggregateID() string { return c.TaskID.String() }

// taskState is the minimal state needed to enforce business rules.
type taskState struct {
    Exists bool
}

// initialState is a function that returns a fresh state — required because
// the framework calls it to create a new instance each time.
var initialState = func() taskState {
    return taskState{}
}

// evolve rebuilds state from a single event. It must be a pure function.
func evolve(state taskState, envelope *eventsourcing.Envelope) taskState {
    switch envelope.Event.(type) {
    case *events.TaskCreated:
        return taskState{Exists: true}
    }
    return state
}

// decide enforces business rules and returns the events to persist.
// Return an error to reject the command.
func decide(state taskState, cmd CreateTask) ([]eventsourcing.Event, error) {
    if state.Exists {
        return nil, fmt.Errorf("task %s already exists", cmd.TaskID)
    }
    if cmd.Title == "" {
        return nil, fmt.Errorf("title is required")
    }

    return []eventsourcing.Event{
        &events.TaskCreated{
            TaskID:      cmd.TaskID,
            Title:       cmd.Title,
            Description: cmd.Description,
            CreatedBy:   cmd.CreatedBy,
            CreatedAt:   time.Now(),
        },
    }, nil
}

// NewHandler wires the three pieces together.
func NewHandler(store eventsourcing.EventStore) eventsourcing.CommandHandler[CreateTask] {
    return eventsourcing.NewCommandHandler(store, initialState, evolve, decide)
}
```

## Step 3: Create the HTTP handler

```go
// slices/createtask/http.go
package createtask

import (
    "net/http"

    "github.com/gin-gonic/gin"
    "github.com/google/uuid"
    "github.com/terraskye/eventsourcing"
)

type HTTPHandler struct {
    handler eventsourcing.CommandHandler[CreateTask]
}

func NewHTTPHandler(handler eventsourcing.CommandHandler[CreateTask]) *HTTPHandler {
    return &HTTPHandler{handler: handler}
}

type request struct {
    Title       string `json:"title" binding:"required"`
    Description string `json:"description"`
}

func (h *HTTPHandler) Handle(c *gin.Context) {
    var req request
    if err := c.ShouldBindJSON(&req); err != nil {
        c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
        return
    }

    cmd := CreateTask{
        TaskID:      uuid.New(),
        Title:       req.Title,
        Description: req.Description,
        CreatedBy:   uuid.New(), // replace with user from auth context
    }

    result, err := h.handler(c.Request.Context(), cmd)
    if err != nil {
        c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
        return
    }

    c.JSON(http.StatusCreated, gin.H{
        "task_id": cmd.TaskID,
        "version": result.NextExpectedVersion,
    })
}

func (h *HTTPHandler) RegisterRoutes(g *gin.RouterGroup) {
    g.POST("", h.Handle)
}
```

## Step 4: Wire it together

```go
// main.go
package main

import (
    "log"

    "github.com/gin-gonic/gin"
    "github.com/terraskye/eventsourcing/eventstore/memory"

    "task-management/slices/createtask"
)

func main() {
    store := memory.NewMemoryStore(100)
    defer store.Close()

    createTaskHandler := createtask.NewHandler(store)
    createTaskHTTP := createtask.NewHTTPHandler(createTaskHandler)

    r := gin.Default()
    tasks := r.Group("/api/v1/tasks")
    createTaskHTTP.RegisterRoutes(tasks)

    log.Fatal(r.Run(":8080"))
}
```

## Step 5: Test it

```bash
go run main.go

# Create a task
curl -X POST http://localhost:8080/api/v1/tasks \
  -H "Content-Type: application/json" \
  -d '{"title": "Learn Event Sourcing", "description": "Complete the tutorial"}'
```

Expected response:

```json
{"task_id": "...", "version": 1}
```

Try submitting without a title:

```bash
curl -X POST http://localhost:8080/api/v1/tasks \
  -H "Content-Type: application/json" \
  -d '{"description": "Missing title"}'
```

Expected: `400 Bad Request`.

## What just happened

1. The HTTP handler created a `CreateTask` command and called `handler(ctx, cmd)`.
2. `NewCommandHandler` loaded events from the store for this stream ID (none, since it's new).
3. It called `evolve` for each event to build `taskState` (initial: `{Exists: false}`).
4. It called `decide(state, cmd)` — our rules passed, so it returned `TaskCreated`.
5. The framework wrapped the event in an `Envelope` and saved it to the store.

## Common mistakes

**Business logic in the HTTP handler** — keep it in `decide`:

```go
// Wrong: validation in HTTP handler
func (h *HTTPHandler) Handle(c *gin.Context) {
    if req.Title == "" { ... }
}

// Right: validation in decide
func decide(state taskState, cmd CreateTask) ([]eventsourcing.Event, error) {
    if cmd.Title == "" {
        return nil, fmt.Errorf("title is required")
    }
    ...
}
```

**Mutating state in decide** — `decide` must be side-effect free:

```go
// Wrong
func decide(state taskState, cmd CreateTask) ([]eventsourcing.Event, error) {
    state.Exists = true // mutating state here has no effect
    ...
}

// Right: return events, the framework calls evolve to update state
func decide(state taskState, cmd CreateTask) ([]eventsourcing.Event, error) {
    return []eventsourcing.Event{&events.TaskCreated{...}}, nil
}
```

**Present-tense event names** — events are facts, not intentions:

```go
type CreateTask struct{} // Wrong: this is a command name
type TaskCreated struct{} // Right: past tense
```

---

Next: [Part 2: Your First Query](./02-first-query.md) — list all tasks using a live read model.

**Exercise**: Add a `DueDate time.Time` field to the command and event. You should not need to change `evolve` or `decide`.
