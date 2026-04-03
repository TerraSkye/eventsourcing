# How to use KurrentDB as the event store

Replace the in-memory store with KurrentDB (formerly EventStoreDB) for durable event persistence.

## Install

```bash
go get github.com/terraskye/eventsourcing/eventstore/kurrentdb
go get github.com/terraskye/eventsourcing/eventbus/kurrentdb
```

## Event store

```go
import (
    "github.com/terraskye/eventsourcing/eventstore/kurrentdb"
)

store, err := kurrentdb.NewEventStore("esdb://localhost:2113?tls=false")
if err != nil {
    log.Fatal(err)
}
defer store.Close()
```

The connection string follows the EventStoreDB URI format. Pass your existing `EventStore` consumers (command handlers, query handlers) the `store` as usual — no changes needed.

## Register events before using the store

KurrentDB serializes events to JSON. You must register all event types before reading or writing:

```go
eventsourcing.RegisterEvent(&events.TaskCreated{})
eventsourcing.RegisterEvent(&events.TaskCompleted{})
eventsourcing.RegisterEvent(&events.TaskArchived{})
```

See [How to register events](./register-events.md) for details.

## Event bus

The KurrentDB event bus uses persistent subscriptions to deliver events reliably, with at-least-once semantics and automatic reconnection.

```go
import (
    kurrentbus "github.com/terraskye/eventsourcing/eventbus/kurrentdb"
)

bus, err := kurrentbus.NewEventBus("esdb://localhost:2113?tls=false")
if err != nil {
    log.Fatal(err)
}
defer bus.Close()

// Subscribe — the subscription name is used as the consumer group name in KurrentDB.
err = bus.Subscribe(ctx, "task-list-projector", projector.EventHandlers())
```

Subscriptions are durable: if your service restarts, KurrentDB resumes from where it left off.

## Filter events per subscriber

The `EventGroupProcessor` implements `StreamFilter()` which returns the list of event type names handled by the group. The KurrentDB event bus uses this to subscribe only to relevant event streams, avoiding unnecessary traffic.

## Differences from in-memory

| Feature | In-memory | KurrentDB |
|---|---|---|
| Persistence | No | Yes |
| Event bus delivery | In-process channel | Persistent subscription |
| `Events()` channel | Yes | No (use bus) |
| Registration required | No | Yes |
| At-least-once delivery | N/A | Yes |

## Running KurrentDB locally

```bash
docker run --rm -d \
  -p 2113:2113 \
  -e EVENTSTORE_INSECURE=true \
  kurrent/kurrentdb:latest --insecure
```

Admin UI: `http://localhost:2113`
