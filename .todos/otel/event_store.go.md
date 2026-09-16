# TODO: `otel/event_store.go`

328 lines. `TelemetryStore` — the instrumented `EventStore` decorator.

## Correctness — this file holds the most serious finding in the package
- [ ] **Unbounded metric cardinality: `stream.id` as a metric attribute.**
      ```go
      EventsAppended.Add(ctx, int64(len(events)), metric.WithAttributes(AttrStreamID.String(streamID)))  // :120
      EventsLoaded.Add(ctx, 1, metric.WithAttributes(AttrStreamID.String(id)))                           // :182, :240
      ```
      `StreamID` is the aggregate ID — **one time series per aggregate, forever**. Every OTel
      cardinality guideline names this exact case; in production it fills the metrics backend and on
      most vendors gets the whole metric dropped. Drop `AttrStreamID` from all metric recordings; it
      is already on the spans, which is the right place. The command/query/event-bus paths get this
      right by using only bounded *type* attributes.
- [ ] **The same instrument is recorded three different ways.** `:182` and `:240` pass a stream ID;
      `:295` passes `metric.WithAttributes()` — no attributes at all. Whatever the fix, make all three
      call sites identical.
- [ ] **`err == io.EOF` should be `errors.Is`** (`:279`; `:167` is fixed). See `iter.go.md` — an `IterFunc`
      that wraps the sentinel is misclassified as a failure.
- [ ] **`Save` mutates the caller's slice** (`:96`): `events[i].Metadata = md`. The map itself is
      copied first, so the caller's map is safe, but the new map is written back into the caller's
      backing array. A telemetry decorator silently rewriting its input is surprising, and it means
      `Save` cannot be retried with the same slice without accumulating propagation headers.
- [ ] **The span context is lost after the first iteration.** In all three `LoadStream*` closures,
      `ctx, rebuildSpan = tracer.Start(ctx, ...)` assigns to the **closure's parameter**, so every
      later invocation records `EventsLoaded` against a context with no active span — losing
      exemplars.

## Structure
- [ ] **`LoadStream`, `LoadStreamFrom` and `LoadFromAll` are three copies of one ~45-line wrapper**,
      and they have drifted: only two nil-check `rebuildSpan`, only two test `err == io.EOF`, only one
      counts events. Extract one helper parameterised by the span attributes.
- [ ] **`started` / `rebuildSpan` / `eventCount` are captured unsynchronised.** Fine for a
      single-consumer iterator, but nothing in `Iterator`'s contract promises that.

## Style
- [ ] `else` after a terminal `if` at `:170`.
- [ ] `causationId` → `causationID` (`:89`). Guide:
      [initialisms](https://google.github.io/styleguide/go/decisions#initialisms).
- [ ] `for _, event := range events { streamID = event.StreamID; break }` (`:58-62`) is
      `if len(events) > 0 { streamID = events[0].StreamID }`.
- [ ] Bare scoping block at `:86-110` — a named helper reads better.
- [ ] Value receivers on `TelemetryStore` while the interface assertion is `(*TelemetryStore)(nil)`
      (`:18`); `TelemetryEventBus` uses pointer receivers. Pick one per package.
- [ ] Span names `"append eventstore"` / `"load eventstore"` — low cardinality and `SpanKindClient` is
      right, but the object should name what is acted on: `"append events"` / `"load events"`.

## Good
- Trace propagation stamped into event metadata on save, so a consumer span can link back to the
  producing trace. This is the hard part of instrumenting an event-sourced system and it is done.
- Spans are started lazily on first iteration, so an unused iterator doesn't emit a span.
- `Close` deliberately delegates without instrumentation, and says so.
