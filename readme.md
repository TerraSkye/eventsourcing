# TerraSkye Eventsourcing

`eventsourcing` is a generic, type-safe event sourcing framework for Go.  
It provides the building blocks for **event-driven architectures**, including command handling, query handling, event buses, envelopes, and a flexible iterator for read models.

This library focuses on simplicity, modern Go patterns, and full generics support, making it easy to build event-sourced systems with strong type guarantees.

---

## Features

- **Commands & Command Handlers** – Strongly typed command routing.
- **Events & Event Handlers** – Publish and consume events safely.
- **Event Bus** – Supports multiple subscribers with typed handlers.
- **Queries & Query Handlers** – Request data through a type-safe query bus.
- **Query Gateway** – Simple façade to dispatch typed queries.
- **Generic Iterator** – Lazy, paginated, or buffered read model iteration.
- **Revision Management** – Built-in support for aggregate and stream revisions.
- **Metadata & Envelopes** – Rich event metadata included by default.
- **Type-Safe Generics Everywhere** – Commands, events, queries, handlers, results.

---

## Installation

```bash
go get github.com/terraskye/eventsourcing
```

---

## OpenTelemetry Instrumentation

The `otel` subpackage provides built-in observability for your event-sourced application using OpenTelemetry standards.

### Why Instrumentation?

Event-sourced systems are inherently distributed and asynchronous. Without proper observability:
- Command failures are hard to diagnose
- Event handler latency goes unnoticed
- Concurrency conflicts are invisible
- Performance bottlenecks in the event store remain hidden

The `otel` package wraps your handlers and stores with tracing spans and metrics, giving you full visibility into your system's behavior without modifying business logic.

### What's Instrumented

| Component | Spans | Metrics |
|-----------|-------|---------|
| Command Handlers | `command.handle <Type>` | duration, in-flight, handled, failed, conflicts |
| Event Handlers | `events.handle <Type>` | duration, handled |
| Event Store | `EventStore.Save`, `EventStore.LoadStream`, etc. | duration, saves, loads, errors, events appended/loaded |

### How to Use

Wrap your handlers and stores with the telemetry decorators:

```go
import "github.com/terraskye/eventsourcing/otel"

// Wrap a command handler
handler := otel.WithCommandTelemetry(myCommandHandler)

// Wrap an event handler
eventHandler := otel.WithEventTelemetry(myEventHandler)

// Wrap an event store
store := otel.WithEventStoreTelemetry(myEventStore)
```

### Configuration Options

Customize span names and attributes using options:

```go
// Static operation name
handler := otel.WithCommandTelemetry(myHandler,
    otel.WithOperation("order.create"),
)

// Add static attributes to all spans
handler := otel.WithCommandTelemetry(myHandler,
    otel.WithAttributes(
        attribute.String("service.name", "orders"),
        attribute.String("service.version", "1.0.0"),
    ),
)

// Dynamic operation name based on context
handler := otel.WithCommandTelemetry(myHandler,
    otel.WithOperationGetter(func(ctx context.Context, defaultOp string) string {
        if tenant := TenantFromContext(ctx); tenant != "" {
            return fmt.Sprintf("%s [%s]", defaultOp, tenant)
        }
        return defaultOp
    }),
)

// Extract dynamic attributes from context
handler := otel.WithCommandTelemetry(myHandler,
    otel.WithAttributeGetter(func(ctx context.Context) []attribute.KeyValue {
        return []attribute.KeyValue{
            attribute.String("tenant.id", TenantFromContext(ctx)),
            attribute.String("user.id", UserFromContext(ctx)),
        }
    }),
)
```

### Available Metrics

**Commands:**
- `eventsourcing.commands.handled` - total successful commands
- `eventsourcing.commands.failed` - total failed commands
- `eventsourcing.commands.duration` - histogram of handling time (ms)
- `eventsourcing.commands.in_flight` - currently processing
- `eventsourcing.concurrency.conflicts` - optimistic locking conflicts

**Events:**
- `eventsourcing.eventbus.handled` - events processed by handlers
- `eventsourcing.eventbus.duration` - handler execution time (ms)
- `eventsourcing.events.appended` - events written to store
- `eventsourcing.events.loaded` - events read from store

**Event Store:**
- `eventsourcing.eventstore.saves` - save operations
- `eventsourcing.eventstore.duration` - operation time (ms)
- `eventsourcing.eventstore.errors` - failed operations

### Semantic Attributes

All spans include semantic attributes following OpenTelemetry conventions:

- `eventsourcing.command.type` - the command type name
- `eventsourcing.aggregate.id` - target aggregate ID
- `eventsourcing.stream.id` - event stream identifier
- `eventsourcing.stream.version` - stream version after operation
- `eventsourcing.event.type` - event type name
- `eventsourcing.event.id` - unique event ID
- `eventsourcing.event.global_position` - global ordering position
- `eventsourcing.event.stream_position` - position within stream

