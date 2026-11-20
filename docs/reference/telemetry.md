# telemetry

OpenTelemetry tracing and metrics for command handlers, query handlers, event handlers, event stores, projections and automations. Every wrapper is a drop-in replacement for what it wraps.

[Add OpenTelemetry tracing](../how-to/add-opentelemetry.md) shows how to wire them up. This page lists every wrapper, span, metric and attribute the package produces.

## Package

```
github.com/terraskye/eventsourcing/telemetry
```

The package is its own Go module: `go get github.com/terraskye/eventsourcing/telemetry`. It uses the global providers (`otel.GetTracerProvider`, `otel.GetMeterProvider`, `otel.GetTextMapPropagator`), so configure those first. Spans and metrics are reported under the instrumentation scope `github.com/terraskye/eventsourcing`, version `eventsourcing.InstrumentationVersion`.

## Wrappers

| Function | Wraps | Returns |
|---|---|---|
| `WithCommandTelemetry[C](next CommandHandler[C], opts ...Option)` | one command handler | `CommandHandler[C]` |
| `CommandTelemetry(opts ...Option)` | every handler on a `CommandBus`, via `Use` | `CommandHandlerMiddleware` |
| `WithQueryTelemetry[T, R](next QueryHandler[T, R], opts ...Option)` | one query handler | `QueryHandler[T, R]` |
| `QueryTelemetry(opts ...Option)` | every handler on a `QueryBus`, via `Use` | `QueryHandlerMiddleware` |
| `WithEventStoreTelemetry(next EventStore, opts ...Option)` | an event store | `EventStore`; also a `GlobalReader` when `next` is one |
| `EventStoreTelemetry(opts ...Option)` | an event store, as middleware | `EventStoreMiddleware` |
| `WithEventTelemetry(next EventHandler, opts ...Option)` | an event handler | `EventHandler` |
| `EventTelemetry(opts ...Option)` | an event handler, as middleware | `EventHandlerMiddleware` |
| `WithProjectionStoreTelemetry(next projection.Store, opts ...Option)` | a projection store | `projection.Store`; also a `DeadLetterStore` when `next` is one |
| `ObserveProjection(r *projection.Runner)` | a projection runner's status | `metric.Registration` |
| `WithWorkTelemetry[T](name string, work automation.WorkFunc[T], opts ...Option)` | an automation's work | `automation.WorkFunc[T]` |
| `WithAutomationStoreTelemetry(next automation.Store, opts ...Option)` | an automation store | `automation.Store` |
| `ObserveAutomation(r StatusReporter)` | an automation runner's status | `metric.Registration` |

`ObserveProjection` and `ObserveAutomation` register a callback that reads `Runner.Status` each time metrics are collected; call `Unregister` on the registration when the runner is no longer run. `StatusReporter` is `interface { Name() string; Status() automation.Status }`, which `*automation.Runner[T]` of any item type satisfies.

The wrapper types `EventStore`, `ProjectionStore` and `AutomationStore` are exported; construct them with the functions above.

## Options

```go
type Option interface { /* unexported */ }

func WithOperation(operation string) Option
func WithOperationGetter(fn func(ctx context.Context, name string) string) Option
func WithAttributes(attrs ...attribute.KeyValue) Option
func WithAttributeGetter(fn func(ctx context.Context) []attribute.KeyValue) Option
```

- `WithOperation` sets a fixed span name. `WithOperationGetter` derives it at call time from the context and the current name; an empty result keeps the current name. Both apply to the command, query, event handler and work spans, and to the event store's `append events` span. `WithProjectionStoreTelemetry` and `WithAutomationStoreTelemetry` ignore them.
- `WithAttributes` adds attributes to every span of the wrapper. `WithAttributeGetter` adds attributes derived from the context at call time to every span of the wrapper.
- Only `WithAttributes` reaches the metrics, and only for the event store: an `AttrDBSystemName` set with it is added to the store's metrics as well as its spans.

## Spans

| Span | Kind | Attributes | Outcome |
|---|---|---|---|
| `handle <command type>` | internal | `eventsourcing.command.type`, `eventsourcing.aggregate.id`, `eventsourcing.correlation.id`, `eventsourcing.causation.id`; once the handler returns, `eventsourcing.stream.id` and `eventsourcing.stream.version` (the `NextExpectedVersion`) | A `BusinessRuleViolationError` leaves the span OK, sets `eventsourcing.error.message` to the cause and adds a `business_rule_violation` event. Any other error fails the span with `error.type`; a `StreamRevisionConflictError`, even one wrapped in a violation, also adds a `concurrency_conflict` event with the stream ID. |
| `handle <query type>` | internal | `eventsourcing.query.type` (the Go type without a leading `*`, such as `queries.GetTask`), `eventsourcing.query.id` | An error fails the span with `error.type`. |
| `append events` | client | `eventsourcing.event_store.operation=append`, `eventsourcing.stream.id`, `eventsourcing.stream.version` (the requested `StreamState` as `ToRawInt64`), the first event's `eventsourcing.correlation.id` and `eventsourcing.causation.id`, plus `WithAttributes` | Before saving, the trace context is injected into every event's `Metadata`, so a later handler can link back. An error fails the span with `error.type`. |
| `load events` | client | `eventsourcing.event_store.operation=load`, `eventsourcing.stream.id`, `eventsourcing.stream.version` (for `LoadStreamFrom`), `eventsourcing.event.count` at the end, plus `WithAttributes` | Runs from the open call until the iterator closes. An error, from opening or from the iterator, fails the span. |
| `read all events` | client | `eventsourcing.event_store.operation=read_all`, `eventsourcing.stream.version` (the `after` position), `eventsourcing.event.count`, plus `WithAttributes` | Created once the read is done, with the read's start time, and only when it returned events or failed: an empty page gets no span. |
| `process event` | internal | `eventsourcing.event.type`, `eventsourcing.event.id`, `eventsourcing.event.global_position`, `eventsourcing.event.stream_position`, `eventsourcing.stream.id`, `eventsourcing.correlation.id`; under a `process batch` span also `eventsourcing.projection.name` and `eventsourcing.projection.segment` | Linked to the span that appended the event when its metadata carries one, with `eventsourcing.link.reason=event.consumed.from.stream`. A `SkippedEventError` leaves the span OK. Any other error fails it with `error.type`. |
| `process batch` | internal | `eventsourcing.projection.name`, `eventsourcing.projection.segment`, `eventsourcing.projection.position` (the new checkpoint), `eventsourcing.projection.phase` | Wraps `projection.Store.Commit`, so the handlers' spans are its children. When the store keeps dead letters, each skipped event adds a `dead letter` event with `eventsourcing.event.global_position`, `eventsourcing.stream.id`, `eventsourcing.event.type` and `eventsourcing.error.message`. |
| `rebuild projection` | internal | `eventsourcing.projection.name`, `eventsourcing.projection.segment` | Wraps `projection.Store.Reset`. |
| `process item` | internal | `eventsourcing.automation.name` | Wraps one call of the automation's `Work`, so a command dispatched through a bus with `CommandTelemetry` is its child. An `automation.Deferred` return leaves the span OK and adds a `deferred` event, with `eventsourcing.automation.deferred_until` (RFC 3339) when set. Any other error fails the span with `error.type`. |
| `pause automation`, `resume automation` | internal | `eventsourcing.automation.name` | Wraps `automation.Store.SetPaused`. |

The automation store also adds a `segment deferred` event to the current span, with `eventsourcing.automation.name`, `eventsourcing.automation.segment` and `eventsourcing.automation.deferred_for` (seconds), on `Defer` and on a `Release` with `DeferFor`; under `WithWorkTelemetry` that is the work's span. Segment keys go on spans only, never on metrics.

Not traced, because they run on every poll: `Head`, the projection store's `Claim`, `Declare`, `Release`, `SaveStatus`, `LoadStatuses`, `Heartbeat`, `LoadInstances` and rebuild and replay bookkeeping, and the automation store's `Acquire`, `Renew`, `Release`, `DeferredFor`, `Paused`, `SaveStatus`, `LoadStatuses` and `LoadSegments`.

## Metrics

Every duration is a histogram in seconds whose count is the number of calls, labelled by `eventsourcing.result`; there are no separate count or error counters. The instruments are exported as package variables so their descriptors can be read when wiring up views or exporters; they are not meant to be recorded to directly.

| Metric | Variable | Instrument | Unit | Attributes |
|---|---|---|---|---|
| `eventsourcing.command.duration` | `CommandDuration` | histogram | `s` | `eventsourcing.command.type`, `eventsourcing.result` (`success`, `rejected`, `failure`), `error.type` on failure |
| `eventsourcing.command.active` | `CommandActive` | up-down counter | `{command}` | `eventsourcing.command.type` |
| `eventsourcing.query.duration` | `QueryDuration` | histogram | `s` | `eventsourcing.query.type`, `eventsourcing.result` (`success`, `failure`), `error.type` on failure |
| `eventsourcing.query.active` | `QueryActive` | up-down counter | `{query}` | `eventsourcing.query.type` |
| `eventsourcing.event_handler.duration` | `EventHandlerDuration` | histogram | `s` | `eventsourcing.event.type`, `eventsourcing.result` (`success`, `skipped`, `failure`), `error.type` on failure; `eventsourcing.projection.name` under a `process batch` span |
| `eventsourcing.event_store.duration` | `EventStoreDuration` | histogram | `s` | `eventsourcing.event_store.operation` (`append`, `load`, `read_all`), `eventsourcing.result` (`success`, `failure`), `error.type` on failure, `db.system.name` when set |
| `eventsourcing.event_store.appended.events` | `EventStoreAppended` | counter | `{event}` | `db.system.name` when set. Counted only when the append succeeded. |
| `eventsourcing.event_store.loaded.events` | `EventStoreLoaded` | counter | `{event}` | `db.system.name` when set. One per event yielded by a load, and the page size of each `ReadAll`. |
| `eventsourcing.projection.batch.duration` | `ProjectionBatchDuration` | histogram | `s` | `eventsourcing.projection.name`, `eventsourcing.result` (`success`, `failure`, `claim_lost`) |
| `eventsourcing.projection.claims` | `ProjectionClaims` | counter | `{claim}` | `eventsourcing.projection.name`, `eventsourcing.result` (`acquired`, `held`, `failure`) |
| `eventsourcing.projection.rebuilds` | `ProjectionRebuilds` | counter | `{rebuild}` | `eventsourcing.projection.name`, `eventsourcing.result` (`success`, `failure`) |
| `eventsourcing.projection.heartbeats` | `ProjectionHeartbeats` | counter | `{heartbeat}` | `eventsourcing.projection.name`, `eventsourcing.result` (`success`, `failure`). The ttl-0 heartbeat a runner leaves with is not counted. |
| `eventsourcing.projection.instance.count` | `ProjectionInstanceCount` | gauge | `{instance}` | `eventsourcing.projection.name`. Every instance reports the same count: aggregate with max. |
| `eventsourcing.projection.segment.held` | `ProjectionSegmentHeld` | gauge | `{segment}` | `eventsourcing.projection.name`. Segments this instance holds as of its last heartbeat. |
| `eventsourcing.projection.lag` | `ProjectionLag` | observable gauge | `{position}` | `eventsourcing.projection.name`. `Head` minus `Position` of `Runner.Status`. A count of events on stores that number events densely; a commit position difference on KurrentDB. |
| `eventsourcing.projection.position` | `ProjectionPosition` | observable gauge | `{position}` | `eventsourcing.projection.name` |
| `eventsourcing.projection.phase` | `ProjectionPhase` | observable gauge | `1` | `eventsourcing.projection.name`, `eventsourcing.projection.phase`: one series per phase, 1 for the current one |
| `eventsourcing.projection.dead_lettered.events` | `ProjectionDeadLettered` | observable counter | `{event}` | `eventsourcing.projection.name`. Reports `Runner.DeadLettered`. |
| `eventsourcing.automation.phase` | `AutomationPhase` | observable gauge | `1` | `eventsourcing.automation.name`, `eventsourcing.automation.phase`: one series per phase, 1 for the current one on this instance |
| `eventsourcing.automation.items` | `AutomationItems` | observable counter | `{item}` | `eventsourcing.automation.name`, `eventsourcing.result` (`success`, `failure`, `deferred`). Reports `Status.Processed`, `Failed` and `Deferred`. |
| `eventsourcing.automation.work.duration` | `AutomationWorkDuration` | histogram | `s` | `eventsourcing.automation.name`, `eventsourcing.result` (`success`, `deferred`, `failure`), `error.type` on failure |
| `eventsourcing.automation.acquisitions` | `AutomationAcquisitions` | counter | `{acquisition}` | `eventsourcing.automation.name`, `eventsourcing.result` (`success`, `failure`) |
| `eventsourcing.automation.segment.grants` | `AutomationSegmentGrants` | counter | `{segment}` | `eventsourcing.automation.name` |
| `eventsourcing.automation.segment.waiting` | `AutomationSegmentWaiting` | gauge | `{segment}` | `eventsourcing.automation.name`. `AcquireResult.Eligible` as of the last acquisition; aggregate with max. |
| `eventsourcing.automation.segment.lost` | `AutomationSegmentLost` | counter | `{segment}` | `eventsourcing.automation.name`. One per lease `Renew` or `Release` reported lost. |

Histogram bucket boundaries, in seconds: the command, batch and work histograms go up to 10s (work to 60s), the others to 5s, starting at 1ms (work at 5ms).

## Attributes

| Constant | Key |
|---|---|
| `AttrDBSystemName` | `db.system.name` |
| `AttrCommandType` | `eventsourcing.command.type` |
| `AttrAggregateID` | `eventsourcing.aggregate.id` |
| `AttrStreamID` | `eventsourcing.stream.id` |
| `AttrStreamVersion` | `eventsourcing.stream.version` |
| `AttrEventType` | `eventsourcing.event.type` |
| `AttrEventID` | `eventsourcing.event.id` |
| `AttrEventCount` | `eventsourcing.event.count` |
| `AttrEventGlobalPos` | `eventsourcing.event.global_position` |
| `AttrEventStreamPos` | `eventsourcing.event.stream_position` |
| `AttrCorrelationID` | `eventsourcing.correlation.id` |
| `AttrCausationID` | `eventsourcing.causation.id` |
| `AttrQueryType` | `eventsourcing.query.type` |
| `AttrQueryID` | `eventsourcing.query.id` |
| `AttrResult` | `eventsourcing.result` |
| `AttrErrorType` | `error.type` |
| `AttrLinkReason` | `eventsourcing.link.reason` |
| `AttrErrorMessage` | `eventsourcing.error.message` |
| `AttrProjectionName` | `eventsourcing.projection.name` |
| `AttrProjectionSegment` | `eventsourcing.projection.segment` |
| `AttrProjectionPosition` | `eventsourcing.projection.position` |
| `AttrProjectionPhase` | `eventsourcing.projection.phase` |
| `AttrAutomationName` | `eventsourcing.automation.name` |
| `AttrAutomationPhase` | `eventsourcing.automation.phase` |
| `AttrAutomationSegment` | `eventsourcing.automation.segment` |
| `AttrEventStoreOperation` | `eventsourcing.event_store.operation` |

`error.type` is set on the span and the duration metric of an operation that failed, never on one that succeeded, and keeps a small, fixed set of values: `stream_revision_conflict`, `stream_exists`, `stream_not_found`, `deadline_exceeded`, `canceled`, or `_OTHER` for any other error.

## Related

- [Add OpenTelemetry tracing](../how-to/add-opentelemetry.md)
- [projection reference](./projection.md) and [automation reference](./automation.md) for the phases the gauges report
