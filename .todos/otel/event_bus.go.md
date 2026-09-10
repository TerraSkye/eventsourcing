# TODO: `otel/event_bus.go`

164 lines. `TelemetryEventBus` — instruments subscriptions registered through it.

## Correctness
- [ ] **Duplicates `WithEventTelemetry` with different behaviour.** `Subscribe`'s inline handler and
      `event_handler.go`'s `WithEventTelemetry` do the same job but diverge:
      | | `Subscribe` | `WithEventTelemetry` |
      |---|---|---|
      | SpanKind | `Consumer` (correct) | `Internal` |
      | `EventBusErrors` | incremented | **not** incremented |
      | Span name | `"receive subscription"` | `"process event"` |
      There is a `TODO` at `event_handler.go:106` asking whether the metric gap is intentional. It
      isn't. Collapse to one implementation.
- [ ] **`EventBusTelemetry` is not the middleware form of this type.** Despite the name pairing with
      `WithEventBusTelemetry`, it returns an `EventHandlerMiddleware` that wraps handlers with
      `WithEventTelemetry` (`:160-164`) — a different code path with the divergences above. The naming
      implies they are interchangeable; they are not.

## OTel conventions
- [ ] **Span name `"receive subscription"`** (`:74`) names the mechanism, not the thing received.
      Messaging convention is `{operation} {destination}` — `"process <event.type>"`.
- [ ] **`attribute.String("eventsourcing.link.reason", "event.consumed.from.stream")`** (`:79`) puts
      dot-namespaced text in an attribute *value*. Keys are namespaced; values are plain. Use
      `"consume"`.

## Style
- [ ] `Subscribe` is one ~65-line function literal passed inline as an argument (`:44-109`). Extract
      it to a named method so the signature and the body are separately readable.
- [ ] Blank line before the closing `}` of `Subscribe` (`:110-111`).

## Good
- **The consumer span is linked back to the producer trace** recovered from event metadata, which is
  what makes command→event tracing work end to end. Correctly uses a span *link* rather than a parent,
  which is right for asynchronous fan-out.
- `SpanKindConsumer` is the correct kind here.
- `ErrSkippedEvent` marks the span `Ok` and skips the error counter — consistent with the `logging`
  package.
- `Errors()` and `Close()` deliberately delegate uninstrumented, and the doc says why.
