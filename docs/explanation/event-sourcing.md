# Event sourcing concepts

## What is event sourcing?

Event sourcing is a pattern for persisting application state: instead of storing the current state of an entity (overwriting it on each change), you store the **sequence of events** that led to that state.

Current state is derived by replaying events:

```
Events:                              State (derived):
  TaskCreated{title:"Buy milk"}  →   {exists:true, title:"Buy milk", done:false}
  TaskCompleted{}                →   {exists:true, title:"Buy milk", done:true}
```

## How it differs from CRUD

| CRUD | Event sourcing |
|---|---|
| Store current state | Store history of changes |
| `UPDATE tasks SET done=true` | Append `TaskCompleted` event |
| Previous state is lost | Full history preserved |
| Single representation | Multiple views from same events |

## The event log

The event log (stream) is the **single source of truth**. It is:

- **Append-only**: events are never modified or deleted.
- **Ordered**: events within a stream have a monotonically increasing version number.
- **Durable**: events survive process restarts.

When you need the current state, you replay all events from the beginning. The `evolve` function is called once per event to fold the event into the current state:

```
state₀ = initialState()
state₁ = evolve(state₀, event₁)
state₂ = evolve(state₁, event₂)
stateₙ = evolve(stateₙ₋₁, eventₙ)   ← current state
```

## Why use event sourcing?

**Complete audit trail**: Every change is recorded with who did it and when. This is required in regulated industries (finance, healthcare) and very useful for debugging.

**Time travel**: Replay events up to any point in time to answer questions like "what was the state of this order on Tuesday?"

**Multiple read models**: The same events can produce different views optimised for different queries. Add a new read model without touching existing code.

**Debugging and replay**: Reproduce bugs by replaying exact events. Fix a projection bug and rebuild it from the event log.

**Decoupled components**: Event producers and consumers are decoupled. A new service can subscribe to existing events without changing the producer.

## Key concepts

### Aggregate

The unit of consistency. All events in a stream belong to one aggregate. Business rules are enforced within an aggregate's boundary. In this library, the `AggregateID()` method identifies which stream an event or command targets.

### Event

An immutable fact about something that happened. Past tense. Contains enough data to reconstruct state. Never references other aggregates by anything other than their ID.

```go
type OrderShipped struct {
    OrderID    uuid.UUID
    TrackingNo string
    ShippedAt  time.Time
}
```

### Command

An expression of intent. Present tense. Can be rejected. Carries the data needed to make the decision.

```go
type ShipOrder struct {
    OrderID    uuid.UUID
    TrackingNo string
}
```

### Stream

The sequence of events for a single aggregate. Identified by a string ID (typically the aggregate's UUID). The event store is a collection of streams.

### Projection / read model

A view derived by replaying events. Can be rebuilt at any time. A projection only handles the events it cares about — others are ignored.

## Tradeoffs

**Complexity**: More moving parts than CRUD. The decide/evolve pattern takes time to learn.

**Eventual consistency**: Projections are updated asynchronously via the event bus. There is a brief window where a query may not reflect the most recent command.

**No in-place updates**: Fixing bad data requires appending a corrective event, not updating existing ones.

**Unbounded streams**: Very long-running aggregates accumulate many events. This is addressed with snapshotting (storing a checkpoint of state) — not yet built into this library.

## Further reading

- [CQRS — separating reads from writes](./cqrs.md)
- [The decide/evolve pattern](./decide-evolve.md)
- [Concurrency and optimistic locking](./concurrency.md)
