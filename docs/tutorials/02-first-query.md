# Part 2: Your First Query

Build a `GET /tasks` endpoint that lists all tasks. You will learn how queries work and how to build a **live read model** by replaying events.

**Time**: ~15 minutes
**Prerequisite**: [Part 1](./01-first-command.md) complete.

## Queries vs commands

| | Command | Query |
|---|---|---|
| Purpose | Change state | Read state |
| Side effects | Persists events | None |
| Returns | `AppendResult` | Your read model |
| Example | `CreateTask` | `ListTasks` |

A **live read model** is rebuilt from events on every query. It's simple and always consistent — but slower for large streams. Use it when the data volume is small or you need guaranteed freshness.

## Step 1: Define the query and read model

```go
// slices/listtasks/query.go
package listtasks

import (
    "context"
    "fmt"

    "github.com/terraskye/eventsourcing"
    "task-management/events"
)

// ListTasks is the query type. ID() is used as a cache key (empty = no specific aggregate).
type ListTasks struct{}

func (q ListTasks) ID() []byte { return []byte("list-tasks") }

// Task is one item in the read model.
type Task struct {
    ID    string `json:"id"`
    Title string `json:"title"`
}

// TaskList is the read model returned by the query.
type TaskList struct {
    Tasks []Task `json:"tasks"`
}

// evolve builds the TaskList from a stream of events.
func evolve(state *TaskList, envelope *eventsourcing.Envelope) *TaskList {
    switch e := envelope.Event.(type) {
    case *events.TaskCreated:
        state.Tasks = append(state.Tasks, Task{
            ID:    e.TaskID.String(),
            Title: e.Title,
        })
    }
    return state
}

// QueryHandler rebuilds the read model on every call.
type QueryHandler struct {
    store eventsourcing.EventStore
}

func NewQueryHandler(store eventsourcing.EventStore) *QueryHandler {
    return &QueryHandler{store: store}
}

func (h *QueryHandler) HandleQuery(ctx context.Context, _ ListTasks) (*TaskList, error) {
    iter, err := h.store.LoadFromAll(ctx, eventsourcing.Any{})
    if err != nil {
        return nil, fmt.Errorf("list tasks: %w", err)
    }

    result := &TaskList{Tasks: make([]Task, 0)}
    for iter.Next(ctx) {
        result = evolve(result, iter.Value())
    }
    if err := iter.Err(); err != nil {
        return nil, err
    }

    return result, nil
}
```

## Step 2: Create the HTTP handler

```go
// slices/listtasks/http.go
package listtasks

import (
    "net/http"

    "github.com/gin-gonic/gin"
)

type HTTPHandler struct {
    queryHandler *QueryHandler
}

func NewHTTPHandler(qh *QueryHandler) *HTTPHandler {
    return &HTTPHandler{queryHandler: qh}
}

func (h *HTTPHandler) Handle(c *gin.Context) {
    result, err := h.queryHandler.HandleQuery(c.Request.Context(), ListTasks{})
    if err != nil {
        c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
        return
    }
    c.JSON(http.StatusOK, result)
}

func (h *HTTPHandler) RegisterRoutes(g *gin.RouterGroup) {
    g.GET("", h.Handle)
}
```

## Step 3: Register the route

Add the query handler to `main.go`:

```go
import "task-management/slices/listtasks"

// After creating store and createTaskHandler:
listTasksHandler := listtasks.NewQueryHandler(store)
listTasksHTTP := listtasks.NewHTTPHandler(listTasksHandler)
listTasksHTTP.RegisterRoutes(tasks) // same router group as createtask
```

## Step 4: Test it

```bash
# Create some tasks
curl -X POST http://localhost:8080/api/v1/tasks \
  -H "Content-Type: application/json" \
  -d '{"title": "Task One"}'

curl -X POST http://localhost:8080/api/v1/tasks \
  -H "Content-Type: application/json" \
  -d '{"title": "Task Two"}'

# List all tasks
curl http://localhost:8080/api/v1/tasks
```

Expected:

```json
{
  "tasks": [
    {"id": "...", "title": "Task One"},
    {"id": "...", "title": "Task Two"}
  ]
}
```

## How it works

`LoadFromAll` returns an iterator over every event ever stored. The query handler walks through all events and applies only the ones it cares about (`TaskCreated`) to build the list.

This is the same `evolve` pattern used in commands — the only difference is the output is a read model rather than aggregate state used for business rules.

## When to use live vs cached

| Situation | Use |
|---|---|
| Small number of events | Live read model (simpler) |
| Large event streams | [Cached projection](../how-to/cached-projection.md) |
| Real-time updates needed | [Cached projection with event bus](./04-projections.md) |
| Eventual consistency is fine | Either |

---

Next: [Part 3: Business Rules](./03-business-rules.md) — enforce rules with the decide/evolve pattern.
