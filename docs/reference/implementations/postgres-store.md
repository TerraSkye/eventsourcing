# postgres.EventStore

A PostgreSQL-backed implementation of `EventStore` using [pgx](https://github.com/jackc/pgx). Durable, supports optimistic concurrency, and safe for multiple concurrent writers.

## Package

```
github.com/terraskye/eventsourcing/postgres
```

## Schema

Apply `postgres.Schema` before use. It creates the tables of the event store, the event bus and the projection store; the same SQL ships as `postgres/schema.sql`. It has the tables as this release uses them and adds no column to a table an earlier release created: when upgrading, run the `ALTER TABLE` statements the changelog lists for each release in between. The events table:

```sql
CREATE TABLE IF NOT EXISTS events (
    id             BIGSERIAL    PRIMARY KEY,
    event_id       UUID         NOT NULL,
    stream_id      VARCHAR      NOT NULL,
    stream_position BIGINT      NOT NULL,
    event_type     VARCHAR      NOT NULL,
    payload        BYTEA        NOT NULL,
    metadata       BYTEA,
    occurred_at    TIMESTAMPTZ  NOT NULL,
    UNIQUE (stream_id, stream_position)
);

CREATE INDEX IF NOT EXISTS idx_events_stream_id ON events (stream_id);
```

## Constructor

```go
func NewEventStore(pool *pgxpool.Pool) *EventStore
```

| Parameter | Description |
|---|---|
| `pool` | A `pgxpool.Pool` connected to a database containing the events table. |

The returned `*EventStore` implements both `EventStore` and `GlobalReader`.

```go
pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
if err != nil {
    log.Fatal(err)
}
store := postgres.NewEventStore(pool)
defer store.Close()
```

`Close` closes the pool — do not share the pool with components that outlive the store.

## Behaviour

- `Save` runs in a transaction and takes a `pg_advisory_xact_lock` on the stream ID to serialize concurrent writes to the same stream. Before inserting, it also takes one advisory lock shared by every append and holds it until commit, so global ids are assigned and committed in order (see below).
- All `StreamState` variants are supported: `Any`, `NoStream`, `StreamExists`, and `Revision`. Version mismatches return a `StreamRevisionConflictError`; a unique-constraint violation on `(stream_id, stream_position)` is mapped to the same error.
- Events are serialized as JSON, so event types must be registered with `RegisterEvent` (see [Event registry](../event-registry.md)); `Save` rejects an unregistered type with `ErrEventNotRegistered`.
- Zero-valued `EventID` and `OccurredAt` fields are filled in on save (`uuid.New()` and `time.Now()`).
- `LoadStream` requires the stream to exist (`StreamExists` semantics).
- `ReadAll` and `Head` (`GlobalReader`) use the global `id` column (`GlobalPosition` on the envelope). Because appends commit one at a time in id order, no id becomes visible before a lower one, so a reader that checkpoints a position never skips an event from a slower concurrent writer. Filters run in SQL; stream prefixes match literally.
- Iterators are lazy: rows are fetched as you iterate and a pool connection is held until the iterator is exhausted or errors. Always drain or abandon iterators promptly.

## Limitations

- Events are inserted one `INSERT` at a time inside the transaction — fine for typical command batches, not tuned for bulk imports.
- Appends to different streams do not commit in parallel: the shared append lock is held from the first insert to commit. Keep append transactions short.
- There is no `Events()` channel; pair the store with [postgres.EventBus](./postgres-eventbus.md), which reads the same table directly.