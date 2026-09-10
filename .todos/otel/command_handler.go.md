# TODO: `otel/command_handler.go`

228 lines. `CommandTelemetry` middleware and `WithCommandTelemetry`.

## Correctness
- [ ] **The two entry points produce different span names for the same operation.**
      `CommandTelemetry` → `"command.handle <pkg.Type>"` (`:43`); `WithCommandTelemetry` →
      `"handle command"` (`:138`). Instrumenting the two supported ways gives two unrelated span
      names. Standardise on `{verb} {object}` — `"handle command"` — and move the type to the
      `eventsourcing.command.type` attribute, which is already set.
- [ ] **Traces and metrics disagree about business rule violations** (`:83-92`, `:201-210`). The span
      is marked `codes.Ok` — an expected domain outcome — while the counter records
      `AttrResult.String("failure")`. The `logging` package makes a third choice (warn). Pick one
      meaning across all three. My read: the span is right; the counter should omit the outcome
      attribute or use a distinct `error.type=business_rule_violation` so it stays visible without
      entering the infrastructure error rate.
- [ ] **A comment describes behaviour the code doesn't have** (`:189`): "Span is still considered
      successful (operation executed)" for a concurrency conflict — but unless the error is *also* a
      business rule violation, control reaches `:215` and sets `codes.Error`.
- [ ] **`var zero C; fmt.Sprintf("%T", zero)`** (`:130-131`) yields `"<nil>"` when `C` is an interface,
      so `eventsourcing.command.type` is mislabelled for interface-instantiated handlers. Same root
      cause as `command_bus.go.md`; resolve together.

## Structure
- [ ] **~90 lines duplicated between the two functions**, and the copies have drifted: the
      `slices.Clone` race fix (`:149`) exists only in the generic form, the span names differ, and the
      typo below exists in only one. The middleware form should call the generic one, the way
      `EventStoreTelemetry` calls `WithEventStoreTelemetry`.

## Style
- [ ] `bussinessViolation` typo (`:194`) — spelled correctly at `:77` in the sibling copy.
- [ ] `else` after a terminal `if` (`:220`). Guide:
      [indent error flow left](https://google.github.io/styleguide/go/decisions#indentation-confusion).
- [ ] `return result, err` at `:226` where `err` is known nil — say `nil`.

## Good
- **`slices.Clone(baseAttributes)` at `:149` with the comment explaining the aliasing race it fixes
  (issue #59)** is exactly the right kind of comment: it stops a future reader "optimising" the clone
  away.
- Metric attributes are correctly limited to the bounded `command.type` — no cardinality problem here,
  unlike `event_store.go`.
- `ErrBusinessRuleViolation` recognised via `errors.As` rather than string matching, and the doc says
  why it is treated as an expected outcome.
