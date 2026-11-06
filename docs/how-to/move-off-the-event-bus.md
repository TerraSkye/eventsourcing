# How to move from the event bus to a projection runner

v0.5.0 removes `EventBus` and its four implementations. Move each subscription to a `projection.Runner`, which reads the same events from the store's global log.

## Why the runner

A bus subscription and a runner both call an `EventHandler` for every new event of the types it asks for. The runner is the durable one:

- It keeps a checkpoint per projection in a projection store, and on SQLite and Postgres commits it in the same transaction as the handler's writes through `sqlite.Tx(ctx)` / `postgres.Tx(ctx)`, so those writes happen exactly once.
- It retries a failing event with backoff instead of reporting it on a channel that drops errors when full, and can skip and dead-letter it instead (`OnError`).
- It rebuilds the read model when you bump `Version`, and spreads one projection over several instances with `Segments`.
- It reports what it is doing with `Status()`, and lets a caller wait for a write with `WaitUntil`.

## Map a subscription to a definition

Before:

```go
bus := postgres.NewEventBus(pool, time.Second)
bus.Use(logging.EventLogging(logger))

err := bus.Subscribe(ctx, "task-list", group,
    postgres.WithFilterEvents(group.StreamFilter()),
)
```

After:

```go
runner := projection.NewRunner(projection.Definition{
    Name:       "task-list",                              // the subscription name
    Handler:    logging.WithEventLogging(logger, group),  // what Use applied
    EventTypes: group.StreamFilter(),                     // the filter option
}, store, postgres.NewProjectionStore(pool))

go runner.Run(ctx)
```

| Bus | Runner |
|---|---|
| The subscription name | `Definition.Name`, which keys the checkpoint |
| The handler | `Definition.Handler` |
| `WithFilterEvents(types)` | `Definition.EventTypes`; for an `EventGroupProcessor`, its `StreamFilter()` |
| `bus.Use(middleware)` | Wrap the handler: `Handler: mw(handler)` |
| `bus.Errors()` | `Definition.OnError` for what happens to a failing event, `Definition.Logger` and `runner.Status()` to see it |
| Closing the subscription's context | Cancelling the context passed to `runner.Run` |

The store argument is the event store, which every store implements `GlobalReader` for. The projection store keeps the checkpoint: `sqlite.NewProjectionStore(db)` or `postgres.NewProjectionStore(pool)` next to a read model in that database, or `memory.NewProjectionStore()` when the checkpoint need not survive a restart, such as a read model kept in memory and rebuilt at startup. KurrentDB has no projection store; keep the checkpoint in one of the others.

## Where the runner starts

A new runner starts from the beginning of the log; the position a bus subscription had reached is not carried over. For a read model the handler can apply twice, such as an upsert, that is a replay and nothing more. For one it cannot, empty the read model first, or set `Version` and `Reset` so the runner empties it itself.

## Latency

The bus woke subscribers on every insert. The runner reads again every `PollInterval`, one second by default, or as soon as something calls `runner.Nudge()`. To keep a read model in the same process as fresh as before, nudge after a successful dispatch, or wait for the write with `runner.WaitUntil(ctx, res.GlobalPosition)`.

See [Build a projection](./cached-projection.md) for the runner in full.
