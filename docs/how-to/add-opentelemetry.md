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

## Instrument a projection

Wrap the three things a `projection.Runner` uses: the event store it reads,
the handler it calls, and the projection store it commits through. Then
register its status metrics:

```go
reader := otel.WithEventStoreTelemetry(store).(eventsourcing.GlobalReader)
pstore := otel.WithProjectionStoreTelemetry(postgres.NewStore(pool))

r := projection.NewRunner(projection.Projection{
    Name:    "task-list",
    Handler: otel.WithEventTelemetry(eventsourcing.NewEventHandlerFunc(list.handle)),
    Events:  []string{"TaskCreated", "TaskCompleted"},
}, reader, pstore)

reg, err := otel.ObserveProjection(r)
if err != nil {
    return err
}
defer reg.Unregister()
```

Each batch becomes a `process batch` span, tagged with
`eventsourcing.projection.name`, `.segment` and `.position`. The handler's
`process event` spans are its children, and each one is still linked to the
span of the command that appended the event. A rebuild becomes a
`rebuild projection` span. Reads that return no events, which a live
projection makes on every poll, are counted in metrics but get no span.

| Metric | Type | What it shows |
|---|---|---|
| `eventsourcing.projection.lag` | gauge | Head of the log minus the checkpoint. Events on Postgres, memory and file; log bytes on KurrentDB |
| `eventsourcing.projection.position` | gauge | The checkpoint |
| `eventsourcing.projection.phase` | gauge | 1 for the current phase, per `eventsourcing.projection.phase` |
| `eventsourcing.projection.events.skipped` | counter | Events skipped under `SkipWhenLive` or `SkipAlways` |
| `eventsourcing.projection.batch.duration` | histogram | Batch commit time, by `eventsourcing.result` (`success`, `failure`, `claim_lost`) |
| `eventsourcing.projection.claims` | counter | Claim attempts, by `eventsourcing.projection.claim.result` |
| `eventsourcing.projection.rebuilds` | counter | Rebuilds, by `eventsourcing.result` |

Alert on `phase{eventsourcing.projection.phase="stalled"} == 1` and on lag
that keeps growing.

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
