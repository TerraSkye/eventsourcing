# Build a projection

A projection keeps a read model up to date from the global event log. Unlike the [live read model](../tutorials/02-first-query.md), which replays events on every query, a projection processes each event once, remembers how far it got, and survives restarts.

The `projection` package provides a `Runner` that does the reading, checkpointing, retrying and coordination between instances. You write the handler.

## How it works

```
         ┌──────────────── Runner ─────────────────┐
log ───► │ ReadAll(after checkpoint, Events) ──►   │
         │   Handler.Handle(ctx, event) ...        │
         │   Store.Commit(read-model writes +      │
         │                checkpoint, one unit)    │
         │ at the head: wait for Nudge or poll     │
         └─────────────────────────────────────────┘
```

A runner needs three things:

1. A `projection.Projection`: the projection's name, its handler, and the event types it handles.
2. An `eventsourcing.GlobalReader` to read the log from. The memory, file, Postgres and KurrentDB event stores all implement it.
3. A `projection.Store` that holds checkpoints, claims and status. Use `projection/memory` for in-memory read models and tests, and `projection/postgres` for read models in Postgres.

## Step 1: Define the projection

A slice that owns a read model has three parts:

- the **read model**: the data the query serves;
- the **projection**: how events turn into that read model;
- the **query**: reads the read model. It never needs the projection.

An in-memory read model is a type both sides share. The projection is a constructor that returns a `projection.Projection` with one handler that switches on the event type:

```go
package tasklist

// TaskList is the read model: written by the projection, read by the query.
type TaskList struct {
    mu    sync.RWMutex
    tasks map[string]*Task
}

func NewProjection(list *TaskList) projection.Projection {
    return projection.Projection{
        Name:    "task-list",
        Handler: eventsourcing.NewEventHandlerFunc(list.handle),
        Events:  []string{"TaskCreated", "TaskCompleted"},
    }
}

func (l *TaskList) handle(ctx context.Context, event eventsourcing.Event) error {
    l.mu.Lock()
    defer l.mu.Unlock()

    switch e := event.(type) {
    case *events.TaskCreated:
        l.tasks[e.TaskID.String()] = &Task{ID: e.TaskID.String(), Title: e.Title}
    case *events.TaskCompleted:
        if t, ok := l.tasks[e.TaskID.String()]; ok {
            t.Completed = true
        }
    }
    return nil
}
```

- `Name` keys the projection's checkpoint, so it must be unique and stable between releases.
- `Handler` is any `eventsourcing.EventHandler`, the same type an event bus subscriber takes.
- `Events` lists the registered names of the event types the handler handles; the runner reads only those. Register each event with `eventsourcing.RegisterEvent` (see [Register events](./register-events.md)); `NewRunner` panics on a name that is not registered, so a typo shows up at startup. Keep `Events` next to the handler, so adding a `case` and its name happen in the same place, and bump `Version` when you do (see [Rebuild a projection](#rebuild-a-projection)).

If you prefer one method per event type, group them and pass the group's filter:

```go
func NewProjection(list *TaskList) projection.Projection {
    group := eventsourcing.NewEventGroupProcessor(
        eventsourcing.OnEvent(list.onTaskCreated),
        eventsourcing.OnEvent(list.onTaskCompleted),
    )
    return projection.Projection{
        Name:    "task-list",
        Handler: group,
        Events:  group.StreamFilter(),
    }
}
```

## Step 2: Run it

`main.go` creates the read model and hands the projection to a runner, together with the event store to read from and a store for the checkpoint:

```go
eventStore := memstore.NewMemoryStore(100)

list := tasklist.NewTaskList()
runner := projection.NewRunner(tasklist.NewProjection(list), eventStore, memory.NewStore()) // projection/memory
queries := tasklist.NewQueryHandler(list)

go runner.Run(ctx)
```

`Run` blocks until `ctx` is done. It never gives up on errors; see [Failures](#failures).

With `memory.NewStore()` the checkpoint lives in memory too, so the projection replays the whole log on every start. That is right for an in-memory read model, which is empty after a restart anyway.

`Projection` is a plain value, so `main.go` can still change how it runs, such as `PollInterval` or a logging wrapper (see [Failures](#failures)), without the slice knowing.

## Store the read model in Postgres

For a durable read model, use `projection/postgres`. It runs every batch in one transaction: the handler's writes and the checkpoint move commit together, so each event is applied exactly once.

Create the tables once, with your migrations or at startup:

```go
_, err := pool.Exec(ctx, postgres.Schema) // projection/postgres
```

Here the read model is the `tasks` table, so the projection keeps no state. The handler writes through the batch's transaction with `postgres.Tx(ctx)`, and the query reads the table with its own connection:

```go
package tasklist

func NewProjection() projection.Projection {
    return projection.Projection{
        Name:    "task-list",
        Handler: eventsourcing.NewEventHandlerFunc(handle),
        Events:  []string{"TaskCreated", "TaskCompleted"},
    }
}

func handle(ctx context.Context, event eventsourcing.Event) error {
    switch e := event.(type) {
    case *events.TaskCreated:
        _, err := postgres.Tx(ctx).Exec(ctx,
            `INSERT INTO tasks (id, title) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`,
            e.TaskID, e.Title)
        return err
    case *events.TaskCompleted:
        _, err := postgres.Tx(ctx).Exec(ctx,
            `UPDATE tasks SET completed = true WHERE id = $1`, e.TaskID)
        return err
    }
    return nil
}

func NewQueryHandler(pool *pgxpool.Pool) *QueryHandler // SELECT ... FROM tasks
```

```go
runner := projection.NewRunner(tasklist.NewProjection(), eventStore, postgres.NewStore(pool))
queries := tasklist.NewQueryHandler(pool)
```

The read model's tables must be in the same database as the store's tables, since they are written in one transaction.

!!! warning "Exactly-once stops at the transaction"
    Only writes through `postgres.Tx(ctx)` are applied exactly once. Anything else a handler does (an email, a webhook, a push to connected browsers) happens **at least once**: when a batch is retried, it happens again. `IsReplaying` does not help here, because a retry is not a replay. Side effects that must not repeat belong in an automation, not in a projection handler.

!!! note
    The memory store cannot roll back a handler's writes. If a handler fails halfway through a batch, the runner retries and some events may be applied twice. Make in-memory handlers safe to apply twice.

## Wait for the read model

A read model is eventually consistent. Two methods let you wait for it:

- `runner.Live(ctx)` reports whether the projection has reached the head of the log. Use it for readiness probes, or to hold off work that must not see a half-built read model.
- `runner.WaitUntil(ctx, globalVersion)` blocks until the projection has processed the log up to that position. Use it for read-your-writes after a command:

```go
res, err := commandBus.Dispatch(ctx, createtask.CreateTask{...})
if err != nil {
    return err
}
if err := runner.WaitUntil(ctx, res.GlobalVersion); err != nil {
    return err
}
// The task is now in the read model.
```

At the head of the log the runner polls every `PollInterval` (1s by default). Call `runner.Nudge()` to make it read right away.

## Rebuild a projection

When you change how a projection writes its read model, rebuild it from the start of the log. Give the projection a `Reset` that wipes the read model, and bump `Version` in the same change as the handler:

```go
func NewProjection() projection.Projection {
    return projection.Projection{
        Name:    "task-list",
        Handler: eventsourcing.NewEventHandlerFunc(handle),
        Events:  []string{"TaskCreated", "TaskCompleted"},
        Version: 2,
        Reset:   reset,
    }
}

func reset(ctx context.Context) error {
    _, err := postgres.Tx(ctx).Exec(ctx, "TRUNCATE tasks")
    return err
}
```

On start, a runner whose stored version differs from `Version` runs `Reset` and the checkpoint move back to zero in one unit of work, then processes the log again. `runner.Rebuild(ctx)` does the same on demand.

!!! warning "Adding an event type is a change too"
    Bump `Version` when you add a name to `Events`, not only when the handler's writes change. Without the bump, the runner carries on from its checkpoint, and every event of the new type stored before the change is never applied.

A rebuild in place empties the read model: queries see an empty table until the runner has caught up again. For a large log, build the new version next to the old one instead; see [Rebuild a projection without downtime](./rebuild-without-downtime.md).

### Skip side effects during a replay

While a projection processes events it has seen before, after a rebuild or on its first run over existing history, `projection.IsReplaying(ctx)` reports true. Use it to skip work that only makes sense for new events:

```go
case *events.TaskCreated:
    // ...update the read model...
    if !projection.IsReplaying(ctx) {
        notifyBrowsers(e)
    }
```

`IsReplaying` skips history; it does not deduplicate. A batch that fails and is retried runs the side effect again (see the warning under [Store the read model in Postgres](#store-the-read-model-in-postgres)).

## Failures

When a batch fails, the runner retries it one event at a time to find the failing event. What happens next depends on `OnError`:

| `OnError` | Behaviour |
|---|---|
| `StallOnError` (default) | The projection stops at the failing event, reports `Stalled`, and retries with backoff (0.5s up to 30s) until the handler succeeds. |
| `SkipWhenLive` | Skips failing events while live; stalls while catching up or rebuilding. |
| `SkipAlways` | Skips failing events in every phase. |

A skipped event does not hold back its stream: the runner keeps applying that stream's later events, so a read model can see `OrderShipped` without the `OrderPlaced` it skipped. Use a skipping policy only when the handlers tolerate such gaps.

Skipped events are recorded in `projection_dead_letters` by the Postgres store. A handler that returns `eventsourcing.SkippedEventError` has declined the event; that is not a failure.

The runner does not log. Wrap the handler to log failures, the same way you would on an event bus. Logging is a deployment choice, so do it in `main.go`:

```go
p := tasklist.NewProjection(list)
p.Handler = logging.WithEventLogging(logger, p.Handler)
runner := projection.NewRunner(p, eventStore, memory.NewStore())
```

`runner.Status()` reports the phase (`Starting`, `Standby`, `CatchingUp`, `Live`, `Stalled`), the position, the head, and the last error.

## Run several instances

Run the same projection in every instance of your service. Each instance competes for the projection's claim through the store; one instance processes the events and the others wait in `Standby`, taking over when the active one stops or its claim expires (`ClaimTTL`, 15s by default). `Live` and `WaitUntil` work on every instance, using the status the active one publishes.

The claim's heartbeat is refreshed when a batch commits, not while its handlers run. Keep `ClaimTTL` above the time your slowest batch takes, or lower `BatchSize` (500 by default) for slow handlers. With the Postgres store a slow batch only delays a takeover: the segment stays locked until the batch commits, and the batch's work is kept. With the memory store another instance can take over halfway through, and the batch's writes are applied anyway.

When one instance cannot keep up, split the projection into segments:

```go
func NewProjection() projection.Projection {
    return projection.Projection{
        Name:       "order-summaries",
        Handler:    eventsourcing.NewEventHandlerFunc(handle),
        Events:     []string{"OrderPlaced", "OrderShipped"},
        Segments:   16,
        SegmentKey: projection.ByStreamID,
    }
}
```

Each event goes to the segment its key maps to, and instances share the segments. Pick a key that keeps every read-model row within one segment; only the slice knows its rows, which is why the segmentation lives in `NewProjection` and not in `main.go`.

Know the limits before you segment:

- **Each segment reads the log.** Every segment reads the filtered log on its own and drops the events of other segments, so 16 segments read the log 16 times. Segment when handlers are the bottleneck, not reading.
- **Each segment writes a heartbeat.** An idle segment refreshes its claim once per `PollInterval`, so 64 segments across many instances keep up a steady write load on the store.
- **Cross-stream read models cannot be segmented.** A row that combines events from many streams, such as a total, a count or a leaderboard, would be written by every segment, and the order between streams is lost. Keep such a projection on one segment. The segment count is fixed per name; changing it takes a new name.

## Write your own Store

A store for another database implements `projection.Store`. Check it against the contract with the acceptance suite:

```go
func TestStoreAcceptance(t *testing.T) {
    projectiontest.StoreAcceptanceTest(t, mystore.New(db)) // projection/projectiontest
}
```

The suite cannot see your read model, so also test that a failed `Commit` rolls back the writes made through your store's transaction.
