# Todolist example

A runnable version of the Task Management API built across the
[tutorials](../../docs/tutorials/index.md):

- [Part 1: Your First Command](../../docs/tutorials/01-first-command.md) — `slices/createtask`
- [Part 2: Your First Query](../../docs/tutorials/02-first-query.md) — live read model, superseded below
- [Part 3: Business Rules](../../docs/tutorials/03-business-rules.md) — `slices/completetask`
- [Part 4: Real-time Projections](../../docs/tutorials/04-projections.md) — `slices/tasklist`
- [Part 5: Background Processing](../../docs/tutorials/05-background-processing.md) — `slices/archivetask`, `slices/taskstoarchive`, `automations/archivetasks`

It follows [Vertical Slice Architecture](../../docs/explanation/vertical-slice-architecture.md):
each feature owns its command/query, `evolve`/`decide`, and HTTP handler.

This is a separate Go module (own `go.mod`, `replace`d to the parent
package, and listed in the repository's `go.work`) so its dependencies —
Gin, for the HTTP layer — don't leak into the main library's `go.mod`.

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

# List tasks — kept up to date by the projection runner
curl -s http://localhost:9000/api/v1/tasks | jq .

# Complete it
curl -X POST "http://localhost:9000/api/v1/tasks/$TASK_ID/complete"

# Completed tasks are archived about 5 seconds later (30 days in production):
# the tasks-to-archive todo list lists them, and the archive-tasks automation
# claims the due ones and sends ArchiveTask.
```

## Layout

```
todolist/
├── main.go                         # wires store, projection runners, automation, HTTP routes
├── events/                         # TaskCreated, TaskCompleted, TaskArchived
├── slices/
│   ├── createtask/                 # POST /tasks
│   ├── completetask/               # POST /tasks/:taskID/complete
│   ├── tasklist/                   # GET /tasks — projection kept up to date by a runner
│   ├── taskstoarchive/             # todo list: completed tasks not archived yet
│   └── archivetask/                # ArchiveTask command, sent by the automation
└── automations/
    └── archivetasks/               # claims due tasks from the todo list, sends ArchiveTask
```
