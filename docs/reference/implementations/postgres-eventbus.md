# postgres.EventBus

A PostgreSQL-backed implementation of `EventBus`. Subscribers poll the `events` table and persist their position in `event_subscriptions`, so subscriptions survive restarts and are safe to run on multiple instances at once. A LISTEN/NOTIFY connection wakes subscribers immediately after a commit, with the poll interval as fallback.

## Package

```
github.com/terraskye/eventsourcing/eventbus/postgres
```

## Schema

Apply `eventbus/postgres/schema.sql` (in addition to the event store schema — the bus reads the same `events` table):

```sql
CREATE TABLE IF NOT EXISTS event_subscriptions (
    name     VARCHAR PRIMARY KEY,
    position BIGINT  NOT NULL DEFAULT 0
);
```

It also installs an `AFTER INSERT` trigger on `events` that sends `pg_notify('eventsourcing_events_inserted', '')`, which is what wakes subscribers without waiting for the next poll.

## Constructor

```go
func NewEventBus(pool *pgxpool.Pool, pollInterval time.Duration) *EventBus
```

| Parameter | Description |
|---|---|
| `pool` | A `pgxpool.Pool` connected to a database containing both tables. |
| `pollInterval` | Fallback polling frequency per subscriber. LISTEN/NOTIFY delivers events immediately in normal operation, so a few seconds is reasonable. |

```go
bus := postgres.NewEventBus(pool, 3*time.Second)
defer bus.Close()
```

There is no `Dispatch` — events become visible to subscribers as soon as they are committed to the `events` table by the event store.

## WithFilterEvents

```go
func WithFilterEvents(types []string) cqrs.SubscriberOption
```

Limits a subscriber to specific event types (matched against the `event_type` column):

```go
bus.Subscribe(ctx, "my-handler", handler,
    postgres.WithFilterEvents([]string{"TaskCreated", "TaskCompleted"}),
)
```

## WithStartFrom

```go
func WithStartFrom(pos int64) cqrs.SubscriberOption
```

Sets the global position a **new** subscription starts from. Only applies when no record exists in `event_subscriptions` yet; existing subscriptions resume from their stored position.

```go
bus.Subscribe(ctx, "new-projection", handler, postgres.WithStartFrom(0))
```

## Behaviour

- Subscriber names are the identity: two processes subscribing with the same name share one position and one of them processes each batch. Each poll locks the subscription row with `FOR UPDATE SKIP LOCKED`, so at most one instance handles events for a given subscription at a time — this gives you competing-consumer semantics across instances for free.
- Events are delivered in global order (`id ASC`), at-least-once. The position is only committed after the batch is handled, so a crash mid-batch redelivers — handlers should be idempotent.
- A handler error aborts the poll and the position is not advanced; the batch is retried on the next cycle. Returning `ErrSkippedEvent` skips the event and continues.
- Polls only see rows committed before the oldest in-progress transaction (`xmin` check), so a slow concurrent writer cannot cause events to be skipped.
- `Use` registers `EventHandlerMiddleware` applied to handlers at `Subscribe` time — call it before subscribing.
- Errors are sent to `Errors()`; if that channel is full, the error is dropped.
- The LISTEN connection reconnects automatically every 5 seconds if it drops. It is an optimization only — polling keeps working without it.
- Locking transactions run on a small dedicated pool (max 4 connections) copied from `pool`, so long-running handlers that need their own connections can't deadlock the bus.
- Cancelling the `Subscribe` context stops the subscriber (its position remains in the table). `Close` stops all subscribers and waits for them to finish; it does **not** close the pool you passed in.

## Limitations

- Delivery latency is bounded by `pollInterval` if LISTEN/NOTIFY is unavailable (e.g. connection poolers that don't support it, such as PgBouncer in transaction mode).
- Batches are capped at 100 events per poll cycle.
- Subscription records are never deleted automatically; remove rows from `event_subscriptions` by hand when retiring a subscriber.