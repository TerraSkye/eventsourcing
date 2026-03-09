# CQRS — separating reads from writes

## What is CQRS?

CQRS (Command Query Responsibility Segregation) separates the parts of a system that **change state** (commands) from the parts that **read state** (queries).

In a traditional architecture, the same data model serves both writes and reads. CQRS lets each side be optimised independently.

## Why separate them?

**Write-side requirements**: Enforce business rules. Handle concurrency. Keep aggregates small and focused. Optimise for correctness.

**Read-side requirements**: Return data fast. Shape data to fit the UI. Join data across multiple aggregates. Optimise for performance.

These requirements often conflict. A normalised data model enforces integrity but requires joins for reads. A denormalised model is fast to read but complex to keep consistent.

CQRS resolves the tension by having two models:

```
                    ┌──────────────────────────────┐
                    │         Application           │
                    └──────────┬───────────┬────────┘
                               │           │
                    Commands   │           │   Queries
                               ▼           ▼
                    ┌──────────────┐  ┌──────────────┐
                    │  Write side  │  │   Read side   │
                    │  (Commands)  │  │  (Projections)│
                    └──────┬───────┘  └──────▲────────┘
                           │                 │
                           │ Events          │ Events
                           ▼                 │
                    ┌──────────────┐         │
                    │  Event Store │─────────┘
                    └──────────────┘
```

## Commands and queries in this library

### Commands

A command targets one aggregate (one stream). The `CommandHandler` loads events for that aggregate, rebuilds state with `evolve`, validates the command with `decide`, and appends new events.

Commands are isolated to their aggregate's stream. They never query read models or other aggregates.

### Queries

A query reads from a projection — a pre-built view optimised for reading. Projections are built by consuming events from the event store (live read model) or kept up-to-date via the event bus (cached projection).

Queries are read-only. They never produce events.

## In practice

The `QueryHandler[T, R]` interface and `QueryBus` handle the query side. The `CommandHandler[C]` type handles the write side. They share nothing except events.

## Eventual consistency

Because projections are updated asynchronously (via the event bus), there is a brief lag between a command completing and its effects being visible in queries. This is called **eventual consistency**.

For most UIs this is acceptable — the write response confirms success, and the UI updates on the next poll. For cases where you need to read what you just wrote, use a live read model that replays events on demand.

## Read model strategies

| Strategy | How built | Latency | Freshness |
|---|---|---|---|
| Live read model | Replay events on each query | Higher | Always current |
| Cached projection | Built once, updated via event bus | Lower | Eventually consistent |
| Query gateway | Routes to registered handler | Depends on handler | Depends on handler |

See [Tutorial Part 2](../tutorials/02-first-query.md) for live read models and [Tutorial Part 4](../tutorials/04-projections.md) for cached projections.
