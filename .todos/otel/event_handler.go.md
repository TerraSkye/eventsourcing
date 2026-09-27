# TODO: `otel/event_handler.go`

117 lines. `WithEventTelemetry` — the per-handler event telemetry decorator.

## Correctness
- [ ] **Does not increment `EventBusErrors`, unlike `TelemetryEventBus.Subscribe`** (`:106-108`). The
      code carries its own `TODO` asking whether that is intentional. It isn't — instrumenting via
      the middleware path silently loses the event error metric. Tracked from the other side in
      `otel/event_bus.go.md`; fix by collapsing the two implementations.
- [ ] **`SpanKindInternal` should be `SpanKindConsumer`** (`:81`). This handler consumes an event
      delivered from elsewhere, and the sibling implementation in `event_bus.go:75` correctly uses
      `Consumer`. The two disagree about what the same work is.
- [ ] **Dead branch** (`:47-49`): the `else` re-runs `make(propagation.MapCarrier)` on a carrier
      already made at `:40`.

## OTel conventions
- [ ] **`attribute.String("eventsourcing.link.reason", "event.consumed.from.stream")`** (`:85`) —
      dot-namespaced text in an attribute value. Same as `event_bus.go`; fix both together.

## Style
- [ ] **`TODO: extract the consumer group`** (`:30`) sits in the doc comment, so it renders on
      pkg.go.dev as part of the public documentation. Move it into the body.
- [ ] `defaultOperation` / `operation` two-step (`:69-78`) is more indirection than the logic needs.

## Good
- The doc comment is explicit that this path does **not** record `EventBusErrors` — the divergence is
  at least documented rather than silent, which is how it became findable.
- Producer-trace link recovered from event metadata, same as the bus form.
- `ErrSkippedEvent` marks the span `Ok`, consistent with the rest of the repo.
