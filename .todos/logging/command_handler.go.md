# TODO: `logging/command_handler.go`

103 lines. `WithCommandLogging` and the `CommandLogging` bus middleware.

## BLOCKER — nil pointer dereference on the generic error path (introduced mid-session)
- [ ] **`conflict.Stream` is dereferenced when `conflict` is nil** (last line of the handler):
      ```go
      var conflict *eventsourcing.StreamRevisionConflictError
      if errors.As(err, &conflict) { ... return result, err }

      l.ErrorContext(ctx, "Dispatch failed", "error", err, "streamID", conflict.Stream, "duration", duration)
      //                                                   ^^^^^^^^ nil whenever errors.As returned false
      ```
      `errors.As` returning false leaves `conflict` nil, so **every non-conflict command error
      panics** — the most common error path in the package.
      `TestWithCommandLogging_ErrorLogsDuration` fails with
      `panic: runtime error: invalid memory address or nil pointer dereference`. `[verified]`

      This looks like a partial application of the "attach `streamID` unconditionally" item below.
      The right source is `result.StreamID`, which is in scope and valid on every path — not
      `conflict`, which only exists inside the branch above.

## Performance
- [ ] **Full logging work happens even when the level is off.** `logger.With(...)` calls
      `Handler.WithAttrs` eagerly and `fmt.Sprintf("%T", command)` allocates — both on every dispatch,
      before anything decides whether the record will be emitted. Measured with the handler at
      `LevelError`, so nothing is written:
      ```
      BenchmarkWithCommandLogging_Disabled-16    752.4 ns/op    488 B/op    13 allocs/op
      BenchmarkRawHandler-16                       0.2 ns/op      0 B/op     0 allocs/op
      BenchmarkLazy_Disabled-16                   56.3 ns/op      0 B/op     0 allocs/op   <- Enabled+LogAttrs
      ```
      13 allocations per command, discarded. For a middleware installed bus-wide this taxes every
      command in the process. Drop `With`, guard with `logger.Enabled(ctx, level)`, emit via
      `LogAttrs` with typed attrs. To keep `%T` off the hot path entirely, wrap the command in a
      `slog.LogValuer`. `[verified]`

## Correctness
- [ ] **Field sets differ per branch, so you can't filter by stream.**
      | branch | fields |
      |---|---|
      | success | `streamID`, `version`, `duration` |
      | business violation | `reason`, `duration` — **no `streamID`, no `error`** |
      | conflict | `streamID` (from `conflict.Stream`, not `result.StreamID`), revisions, `error`, `duration` |
      | other error | `error`, `duration` |
      "Show me everything for stream order-1" silently misses rejections. `result.StreamID` is in
      scope on all four paths — attach it and `error` unconditionally.
- [ ] **A nil logger panics at dispatch time**, inside the handler, not at construction. Exported API:
      default to `slog.Default()` or document that it must be non-nil.

## Style
- [ ] **`duration` means different things per handler.** `"duration", duration` renders as
      `1500000000` under `JSONHandler` and `1.5s` under `TextHandler`; the `otel` package records
      seconds. Use `slog.Duration`, and emit an explicit `duration_ms` if queries need to threshold.
- [ ] **Log-and-return.** Guide:
      [handle an error once](https://google.github.io/styleguide/go/best-practices#error-logging).
      Inherent to a logging middleware and fine, but state the contract in the doc: *this is where
      command errors get logged; don't log them again at the call site.* The root
      `NewCommandHandler` already wraps with command type, aggregate ID and stream ID, so the `error`
      field repeats the structured fields.
- [ ] **Stray blank line at `:33`**, right after the signature. `gofmt` won't remove it; nothing else
      in the package does this.

## Good
- Levels are chosen deliberately and consistently with the `otel` package: a business rule violation
  is `warn`, not `error`, and the doc says why.
- The doc comment enumerates all four outcomes and what each one logs.
