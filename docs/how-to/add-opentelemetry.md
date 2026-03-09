# How to add OpenTelemetry tracing

Wrap command handlers, query handlers, event handlers, and the event store with OpenTelemetry middleware.

## Prerequisites

```bash
go get github.com/terraskye/eventsourcing/otel
go get go.opentelemetry.io/otel
```

## Wrap a command handler

```go
import (
    "github.com/terraskye/eventsourcing/otel"
    oteltrace "go.opentelemetry.io/otel"
)

tracer := oteltrace.Tracer("my-service")

// Assuming createTaskHandler is eventsourcing.CommandHandler[CreateTask]
tracedHandler := otel.TraceCommandHandler(tracer, createTaskHandler,
    otel.WithOperation("CreateTask"),
)
```

Each command execution creates a span named `"CreateTask"` under the current trace.

## Wrap a query handler

```go
tracedQuery := otel.TraceQueryHandler(tracer, queryHandler,
    otel.WithOperation("ListTasks"),
)
```

## Wrap the event store

```go
tracedStore := otel.TraceEventStore(tracer, store,
    otel.WithOperation("EventStore"),
)
```

Pass `tracedStore` to your command/query handlers instead of the raw store.

## Wrap an event handler

```go
tracedEventHandler := otel.TraceEventHandler(tracer, projector.EventHandlers(),
    otel.WithOperation("TaskListProjector"),
)
bus.Subscribe(ctx, "task-list", tracedEventHandler)
```

## Add custom attributes

Use `WithAttributes` for static attributes, or `WithAttributeGetter` for dynamic ones extracted from context:

```go
import "go.opentelemetry.io/otel/attribute"

tracedHandler := otel.TraceCommandHandler(tracer, handler,
    otel.WithOperation("CreateTask"),
    otel.WithAttributes(attribute.String("service.version", "1.0.0")),
    otel.WithAttributeGetter(func(ctx context.Context) []attribute.KeyValue {
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
otel.WithOperationGetter(func(ctx context.Context, operation string) string {
    if userID, ok := ctx.Value("user_id").(string); ok {
        return operation + ":" + userID
    }
    return operation
})
```

## Available otel wrappers

| Function | Wraps |
|---|---|
| `otel.TraceCommandHandler` | `CommandHandler[C]` |
| `otel.TraceQueryHandler` | `QueryHandler[T, R]` |
| `otel.TraceEventStore` | `EventStore` |
| `otel.TraceEventHandler` | `EventHandler` |
| `otel.TraceEventBus` | `EventBus` |
