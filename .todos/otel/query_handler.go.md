# TODO: `otel/query_handler.go`

161 lines. `QueryTelemetry` middleware and `WithQueryTelemetry`.

## Correctness
- [ ] **The two entry points produce different span names.** `QueryTelemetry` →
      `"query.handle <pkg.Type>"` (`:40`); `WithQueryTelemetry` → `"handle query"` (`:123`). Same
      problem and same fix as `otel/command_handler.go.md`: standardise on `{verb} {object}` and let
      `eventsourcing.query.type` carry the type.
- [ ] **`var zero T; fmt.Sprintf("%T", zero)`** (`:90-91`) renders `"<nil>"` when `T` is an interface,
      mislabelling `eventsourcing.query.type`. Note `query_bus.go` already solved exactly this with
      `queryKey`'s `(*T)(nil)` trick — the same fix applies here directly, since this is a static type
      name, not one matched against a runtime value.
- [ ] **`defaultOperation` is overwritten by the getter** (`:129-133`), so the "default" variable ends
      up holding the final value. Cosmetic, but it makes `:135` read as if the getter were ignored.

## Structure
- [ ] **~70 lines duplicated between the middleware and the struct form.** The middleware should call
      `WithQueryTelemetry`. Note this file already has the better structure of the two command/query
      pairs — `telemetryQueryHandler` as a named type is more readable than
      `command_handler.go`'s nested closures.

## Coverage
- [ ] **No result attributes are recorded.** `AttrResultType` and `AttrResultCount` are declared in
      `otel.go` for this file and never used. Either set them here (result type is free; count needs a
      `len`-aware interface) or delete them.

## Style
- [ ] `string(qry.ID())` allocates per query (`:29`, `:115`) — tracked at source in
      `query_handler.go.md`.

## Good
- Metric attributes limited to the bounded `query.type` — no cardinality problem.
- `QueriesProcessing` incremented and decremented with matching attributes via `defer`, so the
  UpDownCounter can never drift.
- The struct form (`telemetryQueryHandler`) is the shape the command file should adopt.
