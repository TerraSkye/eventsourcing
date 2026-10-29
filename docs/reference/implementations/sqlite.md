# sqlite

An event store and a projection store in one SQLite database, built on the pure-Go driver [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite). No server and no C toolchain: a file on disk for development and single-process services, or an in-memory database for tests that need real transactions.

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

if _, err := db.ExecContext(ctx, sqlite.Schema); err != nil {
    log.Fatal(err)
}
```

`Open` limits the database to **one connection**. Writers never meet `SQLITE_BUSY`, and an in-memory database stays one database. For a file it also turns on WAL journaling, begins every transaction with the write lock, and waits up to five seconds for a lock another process holds. `sqlite.Schema` (also shipped as `sqlite/schema.sql`) is idempotent and creates every table below, as this release uses them. It adds no column to a table an earlier release created: when upgrading, run the `ALTER TABLE` statements the changelog lists for each release in between.

## Constructors

```go
func NewEventStore(db *sql.DB) *EventStore
func NewProjectionStore(db *sql.DB) *ProjectionStore
func Tx(ctx context.Context) *sql.Tx
```

Both share `db`, which belongs to the caller: their `Close` methods do not close it.

## Event store

`EventStore` implements `EventStore` and `GlobalReader`. The `events` table's `id` is the global position; SQLite commits one writer at a time, so ids become visible in order and `ReadAll` never skips an event. Streams are read in full before `LoadStream` returns. Events are serialized as JSON, so event types must be registered (see [Event registry](../event-registry.md)); `Save` rejects an unregistered type with `ErrEventNotRegistered`.

## Projection store

`ProjectionStore` runs every batch in one transaction, so a handler's writes and the checkpoint commit together: exactly-once for writes made through `sqlite.Tx(ctx)`. Claims are fenced by owner and epoch.

!!! warning "Write through `sqlite.Tx(ctx)`"
    With one connection, the batch's transaction holds it while the handler runs. A handler that uses `db` directly waits for that connection and never returns.

## When to use something else

SQLite fits one process. When several processes compete for projection segments or share the log, use [postgres](./postgres-store.md) or KurrentDB.
