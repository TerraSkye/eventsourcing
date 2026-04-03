# Part 4: Real-time Projections

Replace the live read model with a **cached projection** that stays up to date via the event bus.

**Time**: ~25 minutes
**Prerequisite**: [Part 3](./03-business-rules.md) complete.

## Why cache projections?

The live read model in Part 2 replays all events on every query. For a small system this is fine, but it becomes slow as events accumulate.

A cached projection:
1. Builds the read model once from existing events.
2. Keeps it updated in real time as new events arrive via the event bus.
3. Serves queries directly from memory — O(1) lookup.

## Architecture

```
Command
  │
  ▼
EventStore ──saves──► Envelope
  │                       │
  │                       ▼
  │                   EventBus ──publishes──► Projector.OnTaskCreated()
  │                                                │
  └─────────────────────────────────────────► cache updated
```

The memory event store has an internal `Events()` channel. You feed this into the event bus, which dispatches to subscribers.

## Step 1: Build the projector

```go
// slices/tasklist/projector.go
package tasklist

import (
    "context"
    "sync"

    "github.com/terraskye/eventsourcing"
    "task-management/events"
)

// Task is the cached read model entry.
type Task struct {
    ID        string `json:"id"`
    Title     string `json:"title"`
    Completed bool   `json:"completed"`
}

// TaskList is the cached read model served by queries.
type TaskList struct {
    Tasks []Task
}

// Projector maintains the cached TaskList.
type Projector struct {
    mu    sync.RWMutex
    tasks map[string]*Task
}

func NewProjector() *Projector {
    return &Projector{tasks: make(map[string]*Task)}
}

// All returns a snapshot of the current task list.
func (p *Projector) All() []Task {
    p.mu.RLock()
    defer p.mu.RUnlock()

    result := make([]Task, 0, len(p.tasks))
    for _, t := range p.tasks {
        result = append(result, *t)
    }
    return result
}

// OnTaskCreated is called by the event bus when a TaskCreated event arrives.
func (p *Projector) OnTaskCreated(_ context.Context, e *events.TaskCreated) error {
    p.mu.Lock()
    defer p.mu.Unlock()
    p.tasks[e.TaskID.String()] = &Task{
        ID:    e.TaskID.String(),
        Title: e.Title,
    }
    return nil
}

// OnTaskCompleted is called by the event bus when a TaskCompleted event arrives.
func (p *Projector) OnTaskCompleted(_ context.Context, e *events.TaskCompleted) error {
    p.mu.Lock()
    defer p.mu.Unlock()
    if t, ok := p.tasks[e.TaskID.String()]; ok {
        t.Completed = true
    }
    return nil
}

// EventHandlers returns the typed handlers to register on the event bus.
func (p *Projector) EventHandlers() *eventsourcing.EventGroupProcessor {
    return eventsourcing.NewEventGroupProcessor(
        eventsourcing.OnEvent(p.OnTaskCreated),
        eventsourcing.OnEvent(p.OnTaskCompleted),
    )
}
```

## Step 2: Write the query handler

```go
// slices/tasklist/query.go
package tasklist

import "context"

type ListTasks struct{}

func (q ListTasks) ID() []byte { return []byte("task-list") }

type QueryHandler struct {
    projector *Projector
}

func NewQueryHandler(p *Projector) *QueryHandler {
    return &QueryHandler{projector: p}
}

func (h *QueryHandler) HandleQuery(_ context.Context, _ ListTasks) ([]Task, error) {
    return h.projector.All(), nil
}
```

## Step 3: Wire the event bus

```go
// main.go
package main

import (
    "context"
    "log"

    "github.com/gin-gonic/gin"
    membus  "github.com/terraskye/eventsourcing/eventbus/memory"
    memstore "github.com/terraskye/eventsourcing/eventstore/memory"

    "task-management/slices/createtask"
    "task-management/slices/completetask"
    "task-management/slices/tasklist"
)

func main() {
    store := memstore.NewMemoryStore(100)
    defer store.Close()

    bus := membus.NewEventBus(100)
    defer bus.Close()

    // Create the projector and register it on the bus.
    projector := tasklist.NewProjector()
    if err := bus.Subscribe(context.Background(), "task-list-projector", projector.EventHandlers()); err != nil {
        log.Fatal(err)
    }

    // Forward events from the store to the bus.
    go func() {
        for env := range store.Events() {
            bus.Dispatch(env)
        }
    }()

    // Handlers
    createTaskHTTP := createtask.NewHTTPHandler(createtask.NewHandler(store))
    completeTaskHTTP := completetask.NewHTTPHandler(completetask.NewHandler(store))
    listTasksHTTP := newTaskListHTTP(tasklist.NewQueryHandler(projector))

    r := gin.Default()
    tasks := r.Group("/api/v1/tasks")
    createTaskHTTP.RegisterRoutes(tasks)
    completeTaskHTTP.RegisterRoutes(tasks)
    listTasksHTTP.RegisterRoutes(tasks)

    log.Fatal(r.Run(":8080"))
}
```

## Step 4: Test it

```bash
# Create tasks
curl -s -X POST http://localhost:8080/api/v1/tasks \
  -H "Content-Type: application/json" \
  -d '{"title": "Buy groceries"}' | jq .

curl -s -X POST http://localhost:8080/api/v1/tasks \
  -H "Content-Type: application/json" \
  -d '{"title": "Write tests"}' | jq .

# List — both tasks appear immediately
curl -s http://localhost:8080/api/v1/tasks | jq .

# Complete one task (use the task_id from above)
curl -X POST "http://localhost:8080/api/v1/tasks/<task_id>/complete"

# List again — completed field is true
curl -s http://localhost:8080/api/v1/tasks | jq .
```

## How `EventGroupProcessor` routes events

`NewEventGroupProcessor` builds a map of event type → handler. When an event arrives via `Handle`, it looks up the correct typed handler by type name and calls it. Unrecognised event types return `ErrSkippedEvent` (not an error — it's expected).

`StreamFilter()` returns the list of event type names the processor handles, which can be used to subscribe only to relevant events in networked event buses.

## Notes on concurrency

The in-memory event bus dispatches events asynchronously in a goroutine per subscriber. The projector uses `sync.RWMutex` to protect the cache. For production use, consider [KurrentDB's event bus](../reference/implementations/kurrentdb-eventbus.md), which provides durable subscriptions.

---

Next: [Part 5: Background Processing](./05-background-processing.md) — trigger commands from events.
