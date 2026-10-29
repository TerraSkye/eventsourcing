# How to add OpenTelemetry tracing

Wrap command handlers, query handlers, event handlers, and the event store with OpenTelemetry middleware.

## Prerequisites

```bash
go get github.com/terraskye/eventsourcing/telemetry
go get go.opentelemetry.io/otel
```

## Wrap a command handler

The wrappers use the global OpenTelemetry tracer and meter providers, so configure those with `otel.SetTracerProvider` and `otel.SetMeterProvider` first.

```go
import "github.com/terraskye/eventsourcing/telemetry"

// Assuming createTaskHandler is eventsourcing.CommandHandler[CreateTask]
tracedHandler := telemetry.WithCommandTelemetry(createTaskHandler)
```

Each command execution creates a span named `handle` and the command type, such as `handle CreateTask`, under the current trace. `telemetry.WithOperation("…")` sets another name. `telemetry.CommandTelemetry()` does the same for every handler on a bus, registered with `CommandBus.Use`.

A command rejected with a `BusinessRuleViolationError` leaves the span OK and is recorded with `eventsourcing.result="rejected"`; any other error fails the span and is recorded as `failure`.

## Wrap a query handler

```go
tracedQuery := telemetry.WithQueryTelemetry(queryHandler)
```

Each query creates a span named `handle` and the query's Go type, such as `handle queries.ListTasks`.

## Wrap the event store

```go
tracedStore := telemetry.WithEventStoreTelemetry(store,
    telemetry.WithAttributes(telemetry.AttrDBSystemName.String("postgresql")),
)
```

Pass `tracedStore` to your command/query handlers instead of the raw store. Its spans are `append events`, `load events` and `read all events`. Set `db.system.name` as above to label the store's spans and metrics with the database behind it; without it they carry none.

## Wrap an event handler

```go
tracedEventHandler := telemetry.WithEventTelemetry(handler)
```

Pass `tracedEventHandler` as the projection's `Handler`. Each call is a `process event` span, linked to the span of the command that appended the event when the event carries one.

## Metrics

Every duration is a histogram in seconds whose count is the number of
calls, labelled by `eventsourcing.result`, so there are no separate
count or error counters.

| Metric | Type | What it shows |
|---|---|---|
| `eventsourcing.command.duration` | histogram | Command handling time, by `eventsourcing.command.type` and `eventsourcing.result` (`success`, `rejected`, `failure`) |
| `eventsourcing.command.active` | up-down counter | Commands being handled |
| `eventsourcing.command.conflicts` | counter | Commands that failed with a concurrency conflict |
| `eventsourcing.query.duration` | histogram | Query handling time, by `eventsourcing.query.type` and `eventsourcing.result` |
| `eventsourcing.query.active` | up-down counter | Queries being handled |
| `eventsourcing.event_handler.duration` | histogram | Event handler time, by `eventsourcing.event.type`, `eventsourcing.result` (`success`, `skipped`, `failure`) and, under a runner, `eventsourcing.projection.name` |
| `eventsourcing.event_store.duration` | histogram | Store operation time, by `eventsourcing.operation` (`save`, `load`, `read_all`), `eventsourcing.result` and `db.system.name` |
| `eventsourcing.event_store.appended.events` | counter | Events appended by saves that succeeded |
| `eventsourcing.event_store.loaded.events` | counter | Events loaded from streams and the global log |

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
| `eventsourcing.projection.dead_lettered.events` | counter | Events dead-lettered under `SkipWhenLive` or `SkipAlways` |
| `eventsourcing.projection.batch.duration` | histogram | Batch commit time, by `eventsourcing.result` (`success`, `failure`, `claim_lost`) |
| `eventsourcing.projection.claims` | counter | Claim attempts, by `eventsourcing.projection.claim.result` |
| `eventsourcing.projection.rebuilds` | counter | Rebuilds, by `eventsourcing.result` |
| `eventsourcing.projection.heartbeats` | counter | Heartbeats an instance sends, by `eventsourcing.result`; while they fail it claims no new segments |
| `eventsourcing.projection.instance.count` | gauge | Live instances of the projection. Every instance reports the same count: aggregate with max |
| `eventsourcing.projection.segment.held` | gauge | Segments this instance holds. Summed over instances it is the segment count |

Alert on `phase{eventsourcing.projection.phase="stalled"} == 1`, on lag
that keeps growing, and on `heartbeats{eventsourcing.result="failure"}`.

## Add custom attributes

Use `WithAttributes` for static attributes, or `WithAttributeGetter` for dynamic ones extracted from context:

```go
import "go.opentelemetry.io/otel/attribute"

tracedHandler := telemetry.WithCommandTelemetry(handler,
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

Use `WithOperationGetter` to set the span name from context at runtime.
Keep the name low-cardinality, such as the API that sent the command:
put IDs (users, tenants, aggregates) in attributes, never in the name,
or every call gets its own span name.

```go
telemetry.WithOperationGetter(func(ctx context.Context, operation string) string {
    if api, ok := ctx.Value(apiKey{}).(string); ok {
        return operation + " via " + api
    }
    return operation
})
```

## Available telemetry wrappers

| Function | Wraps |
|---|---|
| `telemetry.WithCommandTelemetry` | `CommandHandler[C]` |
| `telemetry.CommandTelemetry` | every handler on a `CommandBus`, via `Use` |
| `telemetry.WithQueryTelemetry` | `QueryHandler[T, R]` |
| `telemetry.QueryTelemetry` | every handler on a `QueryBus`, via `Use` |
| `telemetry.WithEventStoreTelemetry` | `EventStore` |
| `telemetry.WithEventTelemetry` | `EventHandler` |
| `telemetry.EventTelemetry` | `EventHandler`, as middleware |
| `telemetry.WithProjectionStoreTelemetry` | `projection.Store` |
