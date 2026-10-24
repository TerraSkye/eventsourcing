# sqlite

An event store, an event bus and a projection store in one SQLite database, built on the pure-Go driver [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite). No server and no C toolchain: a file on disk for development and single-process services, or an in-memory database for tests that need real transactions.

## Package

```
github.com/terraskye/eventsourcing/sqlite
```

The package is its own Go module: `go get github.com/terraskye/eventsourcing/sqlite`.

## Open and apply the schema

```go
db, err := sqlite.Open("events.db") // or ":memory:"
if err != nil {
    log.Fatal(err)
}
defer db.Close()

if err := sqlite.ApplySchema(ctx, db); err != nil {
    log.Fatal(err)
}
```

`Open` limits the database to **one connection**. Writers never meet `SQLITE_BUSY`, and an in-memory database stays one database. For a file it also turns on WAL journaling, begins every transaction with the write lock, and waits up to five seconds for a lock another process holds. `sqlite.ApplySchema` creates every table below, and upgrades a database an earlier release created by adding the columns added since. `sqlite.Schema` (also shipped as `sqlite/schema.sql`) is the plain SQL for your own migrations: it is idempotent, but only creates what is missing, so it adds no column to an existing table.

## Constructors

```go
func NewEventStore(db *sql.DB) *EventStore
func NewEventBus(db *sql.DB, pollInterval time.Duration) *EventBus
func NewProjectionStore(db *sql.DB) *ProjectionStore
func Tx(ctx context.Context) *sql.Tx
```

All three share `db`, which belongs to the caller: their `Close` methods do not close it.

## Event store

`EventStore` implements `EventStore` and `GlobalReader`. The `events` table's `id` is the global position; SQLite commits one writer at a time, so ids become visible in order and `ReadAll` never skips an event. Streams are read in full before `LoadStream` returns.

## Event bus

`EventBus` polls the `events` table every `pollInterval` and keeps each subscription's position in `event_subscriptions`, so a subscription resumes after a restart. Delivery is at-least-once and in id order; a handler that fails is called again on the next poll with the same event. No transaction is held while handlers run, so a handler may append events. Options: `WithFilterEvents`, `WithStartFrom`.

## Projection store

`ProjectionStore` runs every batch in one transaction, so a handler's writes and the checkpoint commit together: exactly-once for writes made through `sqlite.Tx(ctx)`. Claims are fenced by owner and epoch.

!!! warning "Write through `sqlite.Tx(ctx)`"
    With one connection, the batch's transaction holds it while the handler runs. A handler that uses `db` directly waits for that connection and never returns.

## When to use something else

SQLite fits one process. When several processes compete for projection segments or share the log, use [postgres](./postgres-store.md) or KurrentDB.
