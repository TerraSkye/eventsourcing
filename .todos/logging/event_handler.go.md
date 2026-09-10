# TODO: `logging/event_handler.go`

66 lines. `EventLogging` middleware and `WithLoggingMiddleware`.

## API
- [ ] **`WithLoggingMiddleware` is the odd name in the package.** Its siblings are
      `WithCommandLogging` and `WithQueryLogging`; this one matches neither, and "Middleware" as a
      suffix adds nothing the type doesn't already say. Rename to `WithEventLogging`, keeping the old
      name as a deprecated alias.

## Style
- [ ] **Log keys use a third convention.** This file emits `stream-id`, `aggregateId`,
      `global-version`, `event-id` — kebab-case *and* lowerCamel — while
      `logging/command_handler.go` uses `aggregateID`/`streamID`. Three conventions across one
      package, and log keys are a query interface. Pick one (snake_case lines up with the `otel`
      package's attribute names) and apply it to all three files.
- [ ] **Same eager-attribute cost as `command_handler.go`** — `logger.With(...)` with seven context
      lookups per event, evaluated whether or not debug is enabled. This is the highest-frequency
      path in the package (once per event per subscriber), so it matters more here than for commands.
      All seven `*FromContext` calls each walk the context chain; see `context.go.md`.
- [ ] **`cqrs` import alias** for `github.com/terraskye/eventsourcing`, while the other two files in
      this package import it unaliased. Pick one per package.

## Good
- The `switch { case err == nil: ...; case errors.As(err, &skipped): ...; default: }` shape reads
  better than the if-chains in the sibling files — worth making the house style.
- `ErrSkippedEvent` logged at debug, not error, matching the `otel` package's decision to mark that
  span `Ok`. The doc says so explicitly.
