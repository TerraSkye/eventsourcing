# Part 1: Your First Command

In this part, you'll build your first event-sourced feature: **creating a task**. You'll learn how commands, events, and the event store work together.

**Time**: ~30 minutes

**What you'll build**: A working `POST /tasks` endpoint that creates tasks.

## Setup

### 1. Create Project

```bash
mkdir task-management
cd task-management
go mod init task-management
```

### 2. Install Dependencies

```bash
go get github.com/terraskye/eventsourcing
go get github.com/terraskye/eventsourcing/eventstore/memory
go get github.com/gin-gonic/gin
go get github.com/google/uuid
```

### 3. Create Directory Structure

```bash
mkdir -p events slices/createtask
touch main.go
touch events/task_created.go
touch slices/createtask/command.go
touch slices/createtask/http.go
```

Your structure should look like:
```
task-management/
├── main.go
├── events/
│   └── task_created.go
├── slices/
│   └── createtask/
│       ├── command.go
│       └── http.go
└── go.mod
```

## Step 1: Define the Event

Events are **facts about what happened**. Let's create `TaskCreated`.

```go
// events/task_created.go
package events

import (
    "time"
    "github.com/google/uuid"
)

// TaskCreated represents the fact that a task was created
type TaskCreated struct {
    TaskID      uuid.UUID `json:"task_id"`
    Title       string    `json:"title"`
    Description string    `json:"description"`
    CreatedBy   uuid.UUID `json:"created_by"`
    CreatedAt   time.Time `json:"created_at"`
}

// AggregateID implements the Event interface
func (e *TaskCreated) AggregateID() string {
    return e.TaskID.String()
}
```

**💡 Key Points**:
- Past tense: `TaskCreated` (already happened)
- Includes all data: Everything you need to know about what happened
- `AggregateID()`: Identifies which task this event belongs to
- Immutable: Once created, never changed

## Step 2: Create the Command

Commands represent **intent** to do something.

```go
// slices/createtask/command.go
package createtask

import (
    "context"
    "fmt"
    "time"
    
    "github.com/google/uuid"
    "github.com/terraskye/eventsourcing"
    "task-management/events"
)

// CreateTask is the command to create a new task
type CreateTask struct {
    TaskID      uuid.UUID
    Title       string
    Description string
    CreatedBy   uuid.UUID
}

// AggregateID implements the Command interface
func (c CreateTask) AggregateID() string {
    return c.TaskID.String()
}

// taskState represents the current state of a task
type taskState struct {
    Exists bool
}

var initialState = taskState{
    Exists: false,
}

// evolve rebuilds task state from events
func evolve(state taskState, envelope *eventsourcing.Envelope) taskState {
    switch envelope.Event.(type) {
    case *events.TaskCreated:
        return taskState{Exists: true}
    }
    return state
}

// decide determines which events should occur based on the command
func decide(state taskState, cmd CreateTask) ([]eventsourcing.Event, error) {
    // Business rule: Task cannot already exist
    if state.Exists {
        return nil, fmt.Errorf("task %s already exists", cmd.TaskID)
    }
    
    // Business rule: Title is required
    if cmd.Title == "" {
        return nil, fmt.Errorf("title is required")
    }
    
    // All rules passed - create the event
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

// NewHandler creates a command handler for CreateTask
func NewHandler(store eventsourcing.EventStore) eventsourcing.CommandHandler[CreateTask] {
    return eventsourcing.NewCommandHandler(
        store,
        initialState,
        evolve,
        decide,
    )
}
```

**💡 Key Points**:

1. **Command (CreateTask)**:
    - Present tense: `CreateTask` (intent)
    - Includes data needed to execute
    - Can fail validation

2. **State (taskState)**:
    - Tracks if task exists
    - Rebuilt from events via `evolve`

3. **Evolve Function**:
    - Pure function: no side effects
    - Rebuilds state from events
    - `TaskCreated` → `Exists = true`

4. **Decide Function**:
    - Business rules enforced here
    - Returns events or error
    - `decide` = "what should happen?"

5. **Handler**:
    - Wires everything together
    - Framework does the heavy lifting

## Step 3: Create HTTP Handler

Now let's expose this via HTTP using Gin.

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

type CreateTaskRequest struct {
    Title       string `json:"title" binding:"required"`
    Description string `json:"description"`
}

type CreateTaskResponse struct {
    TaskID  string `json:"task_id"`
    Success bool   `json:"success"`
    Version uint64 `json:"version"`
}

func (h *HTTPHandler) Handle(c *gin.Context) {
    var req CreateTaskRequest
    if err := c.ShouldBindJSON(&req); err != nil {
        c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
        return
    }
    
    // Generate IDs
    taskID := uuid.New()
    createdBy := uuid.New() // In real app, get from auth context
    
    // Create command
    cmd := CreateTask{
        TaskID:      taskID,
        Title:       req.Title,
        Description: req.Description,
        CreatedBy:   createdBy,
    }
    
    // Execute command
    result, err := h.handler(c.Request.Context(), cmd)
    if err != nil {
        c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
        return
    }
    
    // Return response
    c.JSON(http.StatusCreated, CreateTaskResponse{
        TaskID:  taskID.String(),
        Success: result.Successful,
        Version: result.NextExpectedVersion,
    })
}

// RegisterRoutes registers the HTTP routes
func (h *HTTPHandler) RegisterRoutes(tasks *gin.RouterGroup) {
    tasks.POST("", h.Handle)
}
```

**💡 Key Points**:
- Gin validation: `binding:"required"`
- Generate UUIDs for TaskID
- Execute command via handler
- Return 201 Created on success
- Route group pattern for clean organization

## Step 4: Wire Everything Together

Now let's create the main application.

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
    // Create event store (in-memory for now)
    store := memory.NewEventStore()
    defer store.Close()
    
    // Create command handler
    createTaskHandler := createtask.NewHandler(store)
    
    // Create HTTP handler
    createTaskHTTP := createtask.NewHTTPHandler(createTaskHandler)
    
    // Setup Gin router
    router := gin.Default()
    
    // API v1 routes
    v1 := router.Group("/api/v1")
    {
        tasks := v1.Group("/tasks")
        {
            createTaskHTTP.RegisterRoutes(tasks)
        }
    }
    
    // Start server
    log.Println("Server starting on :8080")
    log.Println("Create task: POST http://localhost:8080/api/v1/tasks")
    
    if err := router.Run(":8080"); err != nil {
        log.Fatal(err)
    }
}
```

## Step 5: Test It!

### Start the server

```bash
go run main.go
```

You should see:
```
Server starting on :8080
Create task: POST http://localhost:8080/api/v1/tasks
```

### Create a task

```bash
curl -X POST http://localhost:8080/api/v1/tasks \
  -H "Content-Type: application/json" \
  -d '{
    "title": "Learn Event Sourcing",
    "description": "Complete the tutorial"
  }'
```

**Expected response**:
```json
{
  "task_id": "123e4567-e89b-12d3-a456-426614174000",
  "success": true,
  "version": 1
}
```

### Try creating the same task again

```bash
curl -X POST http://localhost:8080/api/v1/tasks \
  -H "Content-Type: application/json" \
  -d '{
    "title": "Another task"
  }'
```

**Success!** Each task gets a unique ID.

### Try without a title

```bash
curl -X POST http://localhost:8080/api/v1/tasks \
  -H "Content-Type: application/json" \
  -d '{
    "description": "Missing title"
  }'
```

**Expected error**:
```json
{
  "error": "Key: 'CreateTaskRequest.Title' Error:Field validation for 'Title' failed on the 'required' tag"
}
```

## What Just Happened?

Let's trace the flow:

```
1. HTTP Request arrives
   POST /api/v1/tasks
   { "title": "Learn ES" }

2. Gin Handler validates request
   ✓ Title is present

3. Create Command
   CreateTask{
     TaskID: <new-uuid>,
     Title: "Learn ES",
     CreatedBy: <user-uuid>
   }

4. Command Handler loads events
   → No events found (new task)
   → State: {Exists: false}

5. Decide function checks business rules
   ✓ Task doesn't exist yet
   ✓ Title is provided
   → Returns: TaskCreated event

6. Event Store saves event
   TaskCreated event persisted

7. HTTP Response
   { "task_id": "...", "success": true, "version": 1 }
```

## Deep Dive: What's in the Event Store?

Even though we're using in-memory storage, events are being persisted. If you had logging enabled, you'd see:

```
Stream: <task-uuid>
Version: 1
Event: TaskCreated{
  TaskID: <task-uuid>,
  Title: "Learn Event Sourcing",
  Description: "Complete the tutorial",
  CreatedAt: 2025-01-09T10:30:00Z
}
```

This is the **source of truth**. We can rebuild any state from these events.

## Common Mistakes

### ❌ Putting logic in HTTP handler
```go
// DON'T DO THIS
func (h *HTTPHandler) Handle(c *gin.Context) {
    // Business logic here
    if title == "" {
        c.JSON(400, gin.H{"error": "title required"})
        return
    }
    // More logic...
}
```

✅ **DO THIS**: Business logic in `decide` function
```go
func decide(state taskState, cmd CreateTask) ([]Event, error) {
    if cmd.Title == "" {
        return nil, fmt.Errorf("title is required")
    }
    // ...
}
```

### ❌ Modifying state in decide
```go
// DON'T DO THIS
func decide(state taskState, cmd CreateTask) ([]Event, error) {
    state.Exists = true  // Mutating state!
    // ...
}
```

✅ **DO THIS**: Return events, framework handles state
```go
func decide(state taskState, cmd CreateTask) ([]Event, error) {
    return []Event{&TaskCreated{...}}, nil
}
```

### ❌ Using present tense for events
```go
// DON'T DO THIS
type CreateTask struct {  // This is a command!
    TaskID string
}
```

✅ **DO THIS**: Past tense for events
```go
type TaskCreated struct {  // Event - already happened
    TaskID string
}
```

## Checkpoint

At this point, you should have:

- ✅ Working `POST /tasks` endpoint
- ✅ Understanding of Commands vs Events
- ✅ Knowledge of `evolve` and `decide` pattern
- ✅ First event persisted in event store

## What's Next?

Right now, we can create tasks but can't see them! In **Part 2**, we'll build a query to list all tasks.

👉 Continue to [Part 2: Your First Query](./part-02-first-query.md)

---

**🎯 Exercise**: Before moving on, try adding a `dueDate` field to tasks:
1. Add `DueDate time.Time` to `CreateTask` command
2. Add it to `TaskCreated` event
3. Add it to the HTTP request
4. Test creating a task with a due date

**💡 Hint**: You don't need to change `evolve` or `decide` - events flow through unchanged!