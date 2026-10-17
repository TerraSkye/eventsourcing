# How to add OpenTelemetry tracing

Wrap command handlers, query handlers, event handlers, and the event store with OpenTelemetry middleware.

## Prerequisites

```bash
go get github.com/terraskye/eventsourcing/telemetry
go get go.opentelemetry.io/otel
```

## Wrap a command handler

```go
import (
    "github.com/terraskye/eventsourcing/telemetry"
    "go.opentelemetry.io/otel"
)

tracer := otel.Tracer("my-service")

// Assuming createTaskHandler is eventsourcing.CommandHandler[CreateTask]
tracedHandler := telemetry.TraceCommandHandler(tracer, createTaskHandler,
    telemetry.WithOperation("CreateTask"),
)
```

Each command execution creates a span named `"CreateTask"` under the current trace.

## Wrap a query handler

```go
tracedQuery := telemetry.TraceQueryHandler(tracer, queryHandler,
    telemetry.WithOperation("ListTasks"),
)
```

## Wrap the event store

```go
tracedStore := telemetry.TraceEventStore(tracer, store,
    telemetry.WithOperation("EventStore"),
)
```

Pass `tracedStore` to your command/query handlers instead of the raw store.

## Wrap an event handler

```go
tracedEventHandler := telemetry.TraceEventHandler(tracer, projector.EventHandlers(),
    telemetry.WithOperation("TaskListProjector"),
)
bus.Subscribe(ctx, "task-list", tracedEventHandler)
```

## Add custom attributes

Use `WithAttributes` for static attributes, or `WithAttributeGetter` for dynamic ones extracted from context:

```go
import "go.opentelemetry.io/otel/attribute"

tracedHandler := telemetry.TraceCommandHandler(tracer, handler,
    telemetry.WithOperation("CreateTask"),
    telemetry.WithAttributes(attribute.String("service.version", "1.0.0")),
    telemetry.WithAttributeGetter(func(ctx context.Context) []attribute.KeyValue {
        if tenantID, ok := ctx.Value("tenant_id").(string); ok {
            return []attribute.KeyValue{attribute.String("tenant.id", tenantID)}
        }
        return nil
    }),
)
```

## Dynamic operation names

Use `WithOperationGetter` to set the span name from context at runtime:

```go
telemetry.WithOperationGetter(func(ctx context.Context, operation string) string {
    if userID, ok := ctx.Value("user_id").(string); ok {
        return operation + ":" + userID
    }
    return operation
})
```

## Available telemetry wrappers

| Function | Wraps |
|---|---|
| `telemetry.TraceCommandHandler` | `CommandHandler[C]` |
| `telemetry.TraceQueryHandler` | `QueryHandler[T, R]` |
| `telemetry.TraceEventStore` | `EventStore` |
| `telemetry.TraceEventHandler` | `EventHandler` |
| `telemetry.TraceEventBus` | `EventBus` |
