# How to use KurrentDB as the event store

Replace the in-memory store with KurrentDB (formerly EventStoreDB) for durable event persistence.

## Install

```bash
go get github.com/terraskye/eventsourcing/kurrentdb
```

## Event store

```go
import (
    "github.com/kurrent-io/KurrentDB-Client-Go/kurrentdb"
    kdb "github.com/terraskye/eventsourcing/kurrentdb"
)

settings, err := kurrentdb.ParseConnectionString("esdb://localhost:2113?tls=false")
if err != nil {
    log.Fatal(err)
}
client, err := kurrentdb.NewClient(settings)
if err != nil {
    log.Fatal(err)
}
store := kdb.NewEventStore(client)
defer store.Close()
```

`NewEventStore(db *kurrentdb.Client, opts ...Option) *EventStore` returns the concrete store, which implements both `EventStore` and `GlobalReader`. The connection string follows the EventStoreDB URI format. Pass your existing `EventStore` consumers (command handlers, query handlers) the `store` as usual — no changes needed.

## Register events before using the store

KurrentDB serializes events to JSON. You must register all event types before reading or writing:

```go
eventsourcing.RegisterEvent(&events.TaskCreated{})
eventsourcing.RegisterEvent(&events.TaskCompleted{})
eventsourcing.RegisterEvent(&events.TaskArchived{})
```

See [How to register events](./register-events.md) for details.

## Projections

`EventStore` implements `GlobalReader`, so a [projection runner](./cached-projection.md) reads KurrentDB's `$all` stream like any other store. KurrentDB has no projection store of its own: keep the checkpoint in another package's, next to the read model it describes.

```go
// pool is a *pgxpool.Pool with postgres.Schema applied.
runner := projection.NewRunner(tasklist.NewProjection(list), store, postgres.NewProjectionStore(pool))
go runner.Run(ctx)
```

The runner reads only the event types in the definition's `EventTypes`; for an `EventGroupProcessor`, pass its `StreamFilter()`.

## Differences from in-memory

| Feature | In-memory | KurrentDB |
|---|---|---|
| Persistence | No | Yes |
| Registration required | Yes | Yes |
| Projection store | Yes | No (use another package's) |

## Running KurrentDB locally

```bash
docker run --rm -d \
  -p 2113:2113 \
  -e EVENTSTORE_INSECURE=true \
  kurrent/kurrentdb:latest --insecure
```

Admin UI: `http://localhost:2113`
