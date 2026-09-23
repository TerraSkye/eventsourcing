# Todolist example

A runnable version of the Task Management API built across the
[tutorials](../../docs/tutorials/index.md):

- [Part 1: Your First Command](../../docs/tutorials/01-first-command.md) — `slices/createtask`
- [Part 2: Your First Query](../../docs/tutorials/02-first-query.md) — live read model, superseded below
- [Part 3: Business Rules](../../docs/tutorials/03-business-rules.md) — `slices/completetask`
- [Part 4: Real-time Projections](../../docs/tutorials/04-projections.md) — `slices/tasklist`
- [Part 5: Background Processing](../../docs/tutorials/05-background-processing.md) — `slices/archivetask`, `processors/archivetasks`

It follows [Vertical Slice Architecture](../../docs/explanation/vertical-slice-architecture.md):
each feature owns its command/query, `evolve`/`decide`, and HTTP handler.

This is a separate Go module (own `go.mod`, `replace`d to the parent
package) so its dependencies — Gin, for the HTTP layer — don't leak into
the main library's `go.mod`.

## Run it

```bash
cd examples/todolist
go run .
```

## Try it

```bash
# Create a task
TASK_ID=$(curl -s -X POST http://localhost:9000/api/v1/tasks \
  -H "Content-Type: application/json" \
  -d '{"title": "Buy groceries"}' | jq -r '.task_id')

# List tasks — updates in real time via the event bus projector
curl -s http://localhost:9000/api/v1/tasks | jq .

# Complete it
curl -X POST "http://localhost:9000/api/v1/tasks/$TASK_ID/complete"

# Completed tasks are auto-archived 5 seconds later (30 days in production —
# see processors/archivetasks) by a background processor reacting to
# TaskCompleted and issuing an ArchiveTask command.
```

## Layout

```
todolist/
├── main.go                        # wires store, event bus, projector, processor, HTTP routes
├── events/                        # TaskCreated, TaskCompleted, TaskArchived
├── slices/
│   ├── createtask/                # POST /tasks
│   ├── completetask/               # POST /tasks/:taskID/complete
│   ├── tasklist/                   # GET /tasks — cached projection kept live via the event bus
│   └── archivetask/                # ArchiveTask command, used only by the processor
└── processors/
    └── archivetasks/               # saga: TaskCompleted -> (delay) -> ArchiveTask
```
