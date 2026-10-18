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

## Instrument a projection

Wrap the three things a `projection.Runner` uses: the event store it reads,
the handler it calls, and the projection store it commits through. Then
register its status metrics:

```go
reader := telemetry.WithEventStoreTelemetry(store).(eventsourcing.GlobalReader)
pstore := telemetry.WithProjectionStoreTelemetry(postgres.NewProjectionStore(pool))

r := projection.NewRunner(projection.Definition{
    Name:       "task-list",
    Handler:    telemetry.WithEventTelemetry(eventsourcing.NewEventHandlerFunc(list.handle)),
    EventTypes: []string{"TaskCreated", "TaskCompleted"},
}, reader, pstore)

reg, err := telemetry.ObserveProjection(r)
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
