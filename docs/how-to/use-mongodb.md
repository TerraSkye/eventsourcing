# How to use MongoDB as the event store

Use the `eventstore/mongodb` and `eventbus/mongodb` packages to back your event-sourced system with MongoDB.

## How it works

Each `Save` atomically writes to two collections inside a single multi-document transaction:

- **`events`** — the source of truth for `LoadStream` and `LoadFromAll`.
- **`outbox`** — an ordered delivery feed consumed by `EventBus`.

A monotonically increasing `global_position` counter (stored in the `counters` collection) is also incremented within the same transaction, so the counter, the events, and the outbox entries are either all committed or all rolled back together.

The `EventBus` polls the `outbox` collection and persists each subscription's last-processed position in a `subscriptions` collection. On restart, every subscription resumes exactly where it left off.

## Requirements

MongoDB 4.0+ on a **replica set** is required for multi-document transactions. A single-node replica set (`mongod --replSet rs0`) works fine for local development. MongoDB Atlas clusters satisfy the requirement out of the box.

## Installation

```bash
go get go.mongodb.org/mongo-driver@latest
go mod tidy
```

## Collections and indexes

`NewEventStore` creates these indexes automatically on startup:

| Collection | Index | Purpose |
|---|---|---|
| `events` | `{ stream_id, stream_position }` unique | Concurrency control, stream reads |
| `events` | `{ global_position }` | `LoadFromAll` |
| `outbox` | `{ global_position }` | EventBus polling |

## Set up the event store

```go
import (
    "context"
    "go.mongodb.org/mongo-driver/mongo"
    "go.mongodb.org/mongo-driver/mongo/options"
    mongostore "github.com/terraskye/eventsourcing/eventstore/mongodb"
)

func main() {
    ctx := context.Background()

    client, err := mongo.Connect(ctx, options.Client().ApplyURI("mongodb://localhost:27017/?replicaSet=rs0"))
    if err != nil {
        log.Fatal(err)
    }

    db := client.Database("myapp")

    store, err := mongostore.NewEventStore(ctx, db)
    if err != nil {
        log.Fatal(err)
    }
    defer store.Close()
    // store implements eventsourcing.EventStore
}
```

## Set up the event bus

Pass the **same** `*mongo.Database` so that the bus reads from the same `outbox` collection the store writes to.

```go
import (
    "time"
    mongobus "github.com/terraskye/eventsourcing/eventbus/mongodb"
)

bus := mongobus.NewEventBus(db, 500*time.Millisecond)
defer bus.Close()
```

## Subscribe to events

```go
err = bus.Subscribe(ctx, "task-projector", projector,
    mongobus.WithFilterEvents([]string{"task.created", "task.completed"}),
)
```

### Start a new subscription from a specific position

```go
bus.Subscribe(ctx, "audit-log", handler,
    mongobus.WithStartFrom(0), // consume from the beginning of the outbox
)
```

`WithStartFrom` only takes effect when no subscription record exists in the database. On subsequent restarts the stored position is used and `WithStartFrom` is ignored.

### Drain async errors

```go
go func() {
    for err := range bus.Errors() {
        log.Println("eventbus error:", err)
    }
}()
```

## Full wiring example

```go
client, _ := mongo.Connect(ctx, options.Client().ApplyURI(uri))
db := client.Database("myapp")

store, _ := mongostore.NewEventStore(ctx, db)
bus := mongobus.NewEventBus(db, 500*time.Millisecond)

handler := eventsourcing.NewCommandHandler(store, initialState, evolve, decide)

bus.Subscribe(ctx, "read-model", projector)

go func() {
    for err := range bus.Errors() {
        log.Println(err)
    }
}()

defer bus.Close()
defer store.Close()
```

## Subscriber options

| Option | Description |
|---|---|
| `WithStartFrom(pos int64)` | Start position for new subscriptions (ignored if a record already exists) |
| `WithFilterEvents([]string{…})` | Receive only events with a matching `event_type` |
