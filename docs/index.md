# terraskye/eventsourcing Documentation

A Go library for building event-sourced systems with CQRS support.

```
go get github.com/terraskye/eventsourcing
```


## Tutorials

Step-by-step guides to learn event sourcing by building a real application.

- [Getting Started](./tutorials/index.md) — overview and prerequisites
- [Part 1: Your First Command](./tutorials/01-first-command.md) — commands, events, event store
- [Part 2: Your First Query](./tutorials/02-first-query.md) — queries and live read models
- [Part 3: Business Rules](./tutorials/03-business-rules.md) — state, validation, and the decide/evolve pattern
- [Part 4: Projections](./tutorials/04-projections.md) — read models kept up to date by the projection runner
- [Part 5: Background Processing](./tutorials/05-background-processing.md) — automations that work off a todo list

A runnable version of everything built across these parts lives in
[`examples/todolist`](https://github.com/TerraSkye/eventsourcing/tree/master/examples/todolist).

---

## How-to guides

Practical recipes for specific tasks.

**Commands**
- [Handle concurrency conflicts with optimistic locking](./how-to/handle-concurrency.md)
- [Add metadata to events](./how-to/add-event-metadata.md)
- [Customise stream names (multi-tenancy)](./how-to/custom-stream-names.md)
- [Retry when you cannot tell whether an append landed](./how-to/retry-an-unknown-outcome.md)
- [The aggregate variant — when to use it and when not to](./how-to/share-state-with-aggregate.md)

**Events**
- [Register events for serialization](./how-to/register-events.md)
- [Version events when the schema changes](./how-to/version-events.md)
- [Read an event's old shape with UnmarshalJSON](./how-to/reshape-events-with-unmarshaljson.md)

**Queries**
- [Build a projection](./how-to/cached-projection.md)
- [Rebuild a projection without downtime](./how-to/rebuild-without-downtime.md)
- [Move from the event bus to a projection runner](./how-to/move-off-the-event-bus.md)

**Infrastructure**
- [Use KurrentDB as the event store](./how-to/use-kurrentdb.md)
- [Add OpenTelemetry tracing](./how-to/add-opentelemetry.md)
- [Use middleware, such as structured logging](./how-to/use-middleware.md)
- [Test event-sourced code](./how-to/test-event-sourced-code.md)

---

## Reference

Complete API documentation.

- [Core interfaces](./reference/interfaces.md) — `Event`, `Command`, `Query`, `EventStore`, `GlobalReader`
- [CommandHandler](./reference/command-handler.md) — `NewCommandHandler`, options
- [EventHandler & EventGroupProcessor](./reference/event-handler.md) — `OnEvent`, `NewEventGroupProcessor`
- [QueryBus & QueryGateway](./reference/query-bus.md) — `NewQueryBus`, `RegisterQueryHandler`, `NewQueryGateway`
- [CommandBus](./reference/command-bus.md) — `NewCommandBus`, `Register`, `Dispatch`
- [Stream states](./reference/stream-states.md) — `Any`, `NoStream`, `StreamExists`, `Revision`
- [Event registry](./reference/event-registry.md) — `RegisterEvent`, `NewEventByName`
- [Serialization](./reference/serialization.md) — the JSON contract every store follows
- [Iterator](./reference/iterator.md) — `Iterator[T]`, `NewIteratorFunc`
- [Context helpers](./reference/context.md) — envelope propagation through context
- [Errors](./reference/errors.md) — error types and sentinel values
- **Implementations**
  - [memory.EventStore](./reference/implementations/memory-store.md)
  - [postgres.EventStore](./reference/implementations/postgres-store.md)
  - [sqlite.EventStore](./reference/implementations/sqlite.md)
  - kurrentdb.EventStore: see [Use KurrentDB as the event store](./how-to/use-kurrentdb.md)

---

## Explanation

Background reading to understand the design and concepts.

- [Event sourcing concepts](./explanation/event-sourcing.md) — what, why, and how events model state
- [CQRS — separating reads from writes](./explanation/cqrs.md)
- [The decide/evolve pattern](./explanation/decide-evolve.md) — how commands become events
- [Concurrency and optimistic locking](./explanation/concurrency.md)
- [Vertical slice architecture](./explanation/vertical-slice-architecture.md) — recommended project layout
- [Personal data and events](./explanation/personal-data.md) — why personal data stays out of the log
- [What the library does not do](./explanation/non-goals.md) — non-goals, and the doors left open
