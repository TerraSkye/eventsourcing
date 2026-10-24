# Part 4: Projections

Replace the live read model with a **projection**: a read model that is built once from the event log and kept up to date as new events are stored.

**Time**: ~25 minutes
**Prerequisite**: [Part 3](./03-business-rules.md) complete.

## Why projections?

The live read model in Part 2 replays every event on every query. For a small system this is fine, but it gets slower as events accumulate.

A projection:

1. Processes each event once and keeps the result, so queries are a lookup.
2. Remembers how far it got in the log (its **checkpoint**), so it catches up on events stored while it was not running.
3. Can be rebuilt from scratch when you change how it builds the read model.

The `projection` package does the reading, checkpointing and retrying for you. You write the projection: the code that turns events into the read model.

## Architecture

```
Command
  │
  ▼
EventStore ──appends──► global log: 1 2 3 4 5 6 ...
                                          │
                       projection.Runner ─┤ reads the events after its checkpoint
                                          ▼
                              projection handler ──► task list updated
                                          │
                                          └─► checkpoint moves forward
```

The runner reads the global log through `ReadAll`, the same call the live read model used in Part 2. When it reaches the end of the log, it checks again every second.

## Step 1: Register the events

The runner only reads the event types you list, and checks at startup that each name is registered. Register the events once, in the `events` package:

```go
// events/register.go
package events

import "github.com/terraskye/eventsourcing"

func init() {
    eventsourcing.RegisterEvent(&TaskCreated{})
    eventsourcing.RegisterEvent(&TaskCompleted{})
}
```

Each event is registered under its `EventType()`: `"TaskCreated"` and `"TaskCompleted"`.

## Step 2: Define the read model

The read model is the data the query serves. Here it lives in memory, so it is a small type that the projection writes and the query reads:

```go
// slices/tasklist/model.go
package tasklist

import "sync"

// Task is one entry in the read model.
type Task struct {
    ID        string `json:"id"`
    Title     string `json:"title"`
    Completed bool   `json:"completed"`
}

// TaskList is the read model: written by the projection, read by the query.
type TaskList struct {
    mu    sync.RWMutex
    tasks map[string]*Task
}

func NewTaskList() *TaskList {
    return &TaskList{tasks: make(map[string]*Task)}
}
```

The projection writes from the runner's goroutine while HTTP requests read, so the map is guarded by a `sync.RWMutex`.

## Step 3: Write the projection

The projection describes how events turn into the read model: a name, a handler, and the event types the handler needs.

```go
// slices/tasklist/projection.go
package tasklist

import (
    "context"

    "github.com/terraskye/eventsourcing"
    "github.com/terraskye/eventsourcing/projection"
    "task-management/events"
)

// NewProjection returns the projection that keeps list up to date.
func NewProjection(list *TaskList) projection.Definition {
    return projection.Definition{
        Name:       "task-list",
        Handler:    eventsourcing.NewEventHandlerFunc(list.handle),
        EventTypes: []string{"TaskCreated", "TaskCompleted"},
    }
}

// handle applies one event to the task list. The runner calls it for every
// event in EventTypes, in the order they were stored.
func (l *TaskList) handle(_ context.Context, event eventsourcing.Event) error {
    l.mu.Lock()
    defer l.mu.Unlock()

    switch e := event.(type) {
    case *events.TaskCreated:
        l.tasks[e.TaskID.String()] = &Task{
            ID:    e.TaskID.String(),
            Title: e.Title,
        }
    case *events.TaskCompleted:
        if t, ok := l.tasks[e.TaskID.String()]; ok {
            t.Completed = true
        }
    }
    return nil
}
```

- `Name` identifies the projection's checkpoint, so it must be unique and stay the same between releases.
- `Handler` is called for every event the runner reads. `NewEventHandlerFunc` turns a function into an `eventsourcing.EventHandler`.
- `EventTypes` lists the event types `handle` switches on. The runner reads only those, so keep it in step with the `case`s; that is why both live in this one file.

## Step 4: Write the query handler

The query reads the read model directly. It never needs the projection.

```go
// slices/tasklist/query.go
package tasklist

import "context"

type ListTasks struct{}

func (q ListTasks) ID() []byte { return []byte("task-list") }

type QueryHandler struct {
    list *TaskList
}

func NewQueryHandler(list *TaskList) *QueryHandler {
    return &QueryHandler{list: list}
}

func (h *QueryHandler) HandleQuery(_ context.Context, _ ListTasks) ([]Task, error) {
    h.list.mu.RLock()
    defer h.list.mu.RUnlock()

    result := make([]Task, 0, len(h.list.tasks))
    for _, t := range h.list.tasks {
        result = append(result, *t)
    }
    return result, nil
}
```

## Step 5: Run the projection

```go
// main.go
package main

import (
    "context"
    "log"

    "github.com/gin-gonic/gin"
    "github.com/terraskye/eventsourcing/memory"
    "github.com/terraskye/eventsourcing/projection"

    "task-management/slices/completetask"
    "task-management/slices/createtask"
    "task-management/slices/tasklist"
)

func main() {
    ctx := context.Background()

    store := memory.NewEventStore(100)
    defer store.Close()

    // The read model, and the runner that keeps it up to date.
    list := tasklist.NewTaskList()
    taskList := projection.NewRunner(tasklist.NewProjection(list), store, memory.NewProjectionStore())

    go func() {
        if err := taskList.Run(ctx); err != nil && ctx.Err() == nil {
            log.Fatal(err)
        }
    }()

    // Handlers
    createTaskHTTP := createtask.NewHTTPHandler(createtask.NewHandler(store))
    completeTaskHTTP := completetask.NewHTTPHandler(completetask.NewHandler(store))
    listTasksHTTP := newTaskListHTTP(tasklist.NewQueryHandler(list))

    r := gin.Default()
    tasks := r.Group("/api/v1/tasks")
    createTaskHTTP.RegisterRoutes(tasks)
    completeTaskHTTP.RegisterRoutes(tasks)
    listTasksHTTP.RegisterRoutes(tasks)

    log.Fatal(r.Run(":8080"))
}
```

`projection.NewRunner` takes three things:

- The **projection** from Step 3.
- The **event store** to read from. The memory store implements `eventsourcing.GlobalReader`.
- A **projection store** to keep the checkpoint in. `memory.NewProjectionStore()` keeps it in memory, which fits a read model that also lives in memory: both start empty on every run, and the runner rebuilds the task list from the start of the log.

`main.go` decides how the projection runs; the slice decides what it does. Because `Definition` is a plain value, `main.go` can still adjust it before passing it on, for example to log handler failures:

```go
p := tasklist.NewProjection(list)
p.Handler = logging.WithEventLogging(logger, p.Handler)
taskList := projection.NewRunner(p, store, memory.NewProjectionStore())
```

## Step 6: Test it

```bash
# Create tasks
curl -s -X POST http://localhost:8080/api/v1/tasks \
  -H "Content-Type: application/json" \
  -d '{"title": "Buy groceries"}' | jq .

curl -s -X POST http://localhost:8080/api/v1/tasks \
  -H "Content-Type: application/json" \
  -d '{"title": "Write tests"}' | jq .

# List: both tasks appear within a second
curl -s http://localhost:8080/api/v1/tasks | jq .

# Complete one task (use the task_id from above)
curl -X POST "http://localhost:8080/api/v1/tasks/<task_id>/complete"

# List again: completed is true
curl -s http://localhost:8080/api/v1/tasks | jq .
```

## Eventual consistency

A projection is **eventually consistent**: a command returns as soon as its events are stored, and the runner picks them up shortly after. In this tutorial that is within a second, the runner's default poll interval.

When a caller must see its own write, wait for the projection to reach the command's position. Every command returns an `AppendResult` whose `GlobalPosition` is the position of its last event:

```go
res, err := createTask(ctx, cmd)
if err != nil {
    return err
}
// Blocks until the task list has processed the new event.
if err := taskList.WaitUntil(ctx, res.GlobalPosition); err != nil {
    return err
}
```

`taskList.IsLive(ctx)` answers a related question: has the projection caught up with the log at all? Use it for a readiness probe, so the service only takes traffic once the task list is built.

If the projection runs in another process, such as a worker, there is no runner to call. Wait on the projection store instead, with a `projection.Probe`; it needs only the store and the projection's name:

```go
probe := projection.NewProbe(projection.ProbeDefinition{Name: "task-list"}, store)
if err := probe.WaitUntil(ctx, res.GlobalPosition); err != nil {
    return err // a *projection.NotReachedError says which segment is behind, and why if it stalled
}
```

## Going further

- **A durable read model.** Keep the task list in Postgres with `postgres.NewProjectionStore`: the handler's writes and the checkpoint commit in one transaction, so a restart resumes where it left off instead of starting over.
- **Rebuilds.** Give the projection a `Version` and a `Reset`; bumping the version rebuilds the read model from the start of the log.
- **Several instances.** Run the same projection in every instance of the service; one processes the events and the others stand by to take over.

[Build a projection](../how-to/cached-projection.md) covers each of these.

---

Next: [Part 5: Background Processing](./05-background-processing.md) — trigger commands from events.
