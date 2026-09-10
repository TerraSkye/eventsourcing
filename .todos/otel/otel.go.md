# TODO: `otel/otel.go`

201 lines. Attribute keys, the shared meter/tracer, and all 13 metric instruments.

## Correctness
- [ ] **Every instrument discards its construction error.** All 13 are built as
      `CommandsCount, _ = meter.Int64Counter(...)`. If an instrument fails to build — bad name, bad
      unit — the SDK returns a working no-op *and* an error explaining why. Swallowing it means the
      metric silently produces nothing forever. Given that several names below are wrong, this is the
      mechanism that would hide a rename typo. Build them in a helper that joins the errors and
      either panics at startup or exposes a package-level `Err`.

## OTel conventions — the Counter and the UpDownCounter have swapped naming
- [ ] | Instrument | Current | Rule | Should be |
      |---|---|---|---|
      | `Int64Counter` | `eventsourcing.commands.count` | Counters pluralize the countable noun; `.count` is the **UpDownCounter** form | `eventsourcing.command.operations` |
      | `Float64Histogram` | `eventsourcing.commands.duration` | Durations are not pluralized (`http.server.request.duration`) | `eventsourcing.command.duration` |
      | `Int64UpDownCounter` | `eventsourcing.command.processing` | UpDownCounters use `.count` (`system.process.count`) | `eventsourcing.command.count` |
      All three apply verbatim to `queries.count` / `queries.duration` / `query.processing`.
      Independent of the convention, `commands.*` and `command.processing` are two namespaces for one
      subject, so no dashboard can glob them together.
- [ ] **`eventbus` / `eventstore` should be `event_bus` / `event_store`** — segments use snake_case for
      compound words (`http.response.status_code`).
- [ ] **Drop the parallel `.errors` counters.** `eventsourcing.eventstore.errors` and
      `eventsourcing.eventbus.errors` cannot be correlated with their non-error siblings; the
      convention is an `error.type` attribute on the primary counter.
- [ ] **`AttrResult` ("eventsourcing.result" = `"success"`/`"failure"`) should be `error.type`.** It
      breaks the `{object}.{property}` attribute pattern (`result` is a bare property), and the
      convention is to omit the attribute on success and set `error.type` on failure so the error
      rate is `sum(rate) by (error.type)`. `AttrErrorType` is already declared for this and unused.

## API
- [ ] **Nine exported attribute keys are dead** — declared here, referenced nowhere else:
      `AttrErrorType`, `AttrRetryCount`, `AttrRetryMax`, `AttrHandlerName`, `AttrConflictType`,
      `AttrShardID`, `AttrQueueDepth`, `AttrResultType`, `AttrResultCount`. Exported means public API
      forever. Wire them up (`AttrErrorType` especially) or remove them now. `[verified]`
- [ ] **`AttrDBSystem = "db.system"` is superseded by `db.system.name`**, and hand-declaring semconv
      keys means they never track upstream renames. Import
      `go.opentelemetry.io/otel/semconv/v1.x.x` instead. Guide: don't redefine what a dependency
      provides.
- [ ] **No `WithMeterProvider` / `WithTracerProvider`.** `meter` and `tracer` are package vars bound to
      the **global** providers at import time (`:66-69`). Every OTel instrumentation library —
      `otelhttp`, `otelgrpc`, `otelsql` — offers these options, because binding to the global makes
      the package untestable against a local `SpanRecorder` and unusable with more than one provider.
      `otel/config.go`'s `Option` type is already the right vehicle. Moving instruments to per-wrapper
      construction also resolves the discarded errors above, since construction would then be able to
      return one.
- [ ] **The instruments are exported `var`s**, so a caller can reassign `CommandsCount` and silently
      redirect the package's telemetry. If they must stay reachable for descriptor introspection,
      expose them through an accessor.

## Good
- **Units are correct throughout** — `{command}`, `{event}`, `{query}`, `{error}`, `{conflict}`,
  `{operation}` are properly singular curly-brace annotations, and durations use `s`. This is the part
  people usually get wrong.
- Histogram bucket boundaries are sensible and consistent across the three histograms.
- Attribute keys are namespaced under `eventsourcing.` and grouped by subject with section comments.
