# TODO: `logging/command_handler.go`

122 lines. `WithCommandLogging` and the `CommandLogging` bus middleware.

_Re-evaluated 2026-09-11 (second pass) against the current file — the source keeps changing
mid-session, so the statuses below supersede both earlier passes._

## BLOCKER — `streamID` is empty on the conflict path (regression)
- [ ] **`result.StreamID` is not populated on the conflict path.** The conflict branch now reads
      `streamID` from `result.StreamID` (`:92`), but an [EventStore] is not obliged to fill the
      result in when it returns a conflict, and the in-memory one does not:
      ```go
      // eventstore/memory/eventstore.go:140
      return eventsourcing.AppendResult{}, &eventsourcing.StreamRevisionConflictError{...}
      ```
      `eventstore/file` does set it (`AppendResult{StreamID: streamID, Successful: false}`), so the
      field is populated with some stores and empty with others — exactly the inconsistency this
      item set out to remove.
      `TestWithCommandLogging_ConflictLogsRevisions` fails with `streamID = , want "order-1"`.
      `[verified]`

      The earlier note in this file claimed `result.StreamID` "is in scope and valid on every path".
      The first half is true, the second is not: on the error paths it is only as good as the store.
      `conflict.Stream` is always set by every store, so the conflict branch should prefer it and
      fall back to the result:
      ```go
      streamID := conflict.Stream
      if streamID == "" {
          streamID = result.StreamID
      }
      ```

## BLOCKER — `errors.AsType` outruns the module's Go version
- [ ] **`errors.AsType` requires go1.26; `go.mod` declares `go 1.25.0`** (`:60`).
      `go vet ./logging/` fails:
      ```
      logging/command_handler.go:60:30: errors.AsType requires go1.26 or later (file is go1.25)
      ```
      It builds here only because the local toolchain is go1.26.1 — on a go1.25 toolchain the symbol
      does not exist and the package will not compile. Either raise `go.mod` to 1.26 deliberately
      (it is a library; that pushes the floor onto every consumer) or go back to
      `var violation *eventsourcing.ErrBusinessRuleViolation; errors.As(err, &violation)`, which is
      what the conflict branch four lines below still uses. Mixing the two idioms in one function is
      also its own inconsistency. `[verified]`

## Correctness
- [ ] **Logging `err` destroys the message when the revisions are nil.** The branch carefully guards
      `ExpectedRevision`/`ActualRevision` against nil before rendering them (`:81`-`:87`) and then
      passes the raw `err` anyway (`:90`). `StreamRevisionConflictError.Error` calls
      `ToRawInt64()` on both, so under a `TextHandler` the attr renders as:
      ```
      error="%!v(PANIC=Error method: runtime error: invalid memory address or nil pointer dereference)"
      ```
      `fmt` recovers the panic, so nothing crashes — but the one field that says what went wrong is
      gone. `JSONHandler` escapes it (it marshals the struct's exported fields instead of calling
      `Error`), so the existing `TestWithCommandLogging_ConflictWithoutRevisions` passes and the bug
      only shows on text output. Either guard `Error()` itself in `errors.go` or log
      `conflict.Stream` + revisions and drop the redundant `error` attr on this branch. `[verified]`
- [ ] **`error` is still missing on the business-violation branch.** `streamID` is now on all four
      branches (below), but the warn branch logs `reason` and no `error` (`:65`-`:72`), so an
      error-field query still misses rejections.
- [x] **Field sets differ per branch, so you can't filter by stream.**
      *Done for `streamID`: all four branches now attach it (`:50`, `:68`, `:92`, `:104`). Note the
      conflict branch attaches an empty one — see the first blocker.*
- [x] **A nil logger panics at dispatch time**, inside the handler, not at construction. Exported API:
      default to `slog.Default()` or document that it must be non-nil.
      *Done: falls back to `slog.Default()` when `logger == nil` (`:33`-`:35`).*

## Performance
- [x] **Full logging work happens even when the level is off.** `logger.With(...)` called
      `Handler.WithAttrs` eagerly and `fmt.Sprintf("%T", command)` allocated on every dispatch,
      before anything decided whether the record would be emitted.
      *Done: `With` is gone and both info calls sit behind `l.Enabled(ctx, slog.LevelInfo)`
      (`:38`, `:48`), which keeps the `Sprintf` off the disabled path. Re-measured 2026-09-11,
      handler at `LevelError`:*
      ```
      before:  1284 ns/op   505 B/op   14 allocs/op
      after:   56.7 ns/op     0 B/op    0 allocs/op   <- matches the LogAttrs target
      ```
      `[verified]`
- [ ] **The enabled path still boxes every attr.** 2067 ns/op, 136 B/op, 8 allocs/op at
      `LevelInfo` (two records per dispatch). The `...any` variadic form boxes each key and value;
      `LogAttrs` with typed `slog.String`/`slog.Uint64`/`slog.Duration` attrs avoids that. Lower
      priority than the disabled path — this one at least produces output for the money.
      `[verified]`
- [ ] **The two error branches still pay `fmt.Sprintf("%T", command)` unconditionally** (`:96`,
      `:106`). Arguments are evaluated before `ErrorContext` gets a chance to check the level, so
      the guard that now protects the info paths doesn't cover these. A `slog.LogValuer` wrapper
      around the command would make `%T` lazy everywhere and remove the need to guard each call.

## Style
- [ ] **`duration` means different things per handler.** `JSONHandler` renders it as `1500000000`,
      `TextHandler` as `1.5s`; the `otel` package records seconds.
      *Partly done: the value is a `slog.DurationValue` rather than a bare `time.Duration`,
      so it is typed consistently on every branch (`:46`). Still open: that is still nanoseconds under
      `JSONHandler`, so an explicit `duration_ms` is needed if logs are to be thresholded.*
- [ ] **Log-and-return.** Guide:
      [handle an error once](https://google.github.io/styleguide/go/best-practices#error-logging).
      Inherent to a logging middleware and fine, but state the contract in the doc: *this is where
      command errors get logged; don't log them again at the call site.* The root
      `NewCommandHandler` already wraps with command type, aggregate ID and stream ID, so the `error`
      field repeats the structured fields.
- [ ] **Two stray blank lines now**: `:37`, right after `return func(...) {`, and `:66`, in the
      middle of the `WarnContext` argument list between the message and the first key. *The first
      was fixed earlier in the session and came back; the second is new.* `gofmt` leaves both;
      nothing else in the package does this.
- [ ] **`command` and `aggregateID` are repeated verbatim in five call sites** (`:40`, `:53`,
      `:70`, `:96`, `:106`). Removing `With` fixed the cost but moved the duplication into the
      body — five places to keep in sync, and the `Sprintf` now runs once per branch instead of
      once per dispatch. Computing the pair once into a `[]slog.Attr` (or a `slog.LogValuer` on
      the command) keeps the laziness without the copy-paste.
- [ ] **The receiver-ish parameter is now `l`, but `CommandLogging` below still calls it `logger`**
      (`:32` vs `:118`). `l` reads as a local, not as an exported function's parameter, and it is
      what shows up in the godoc signature. Pick one name.

## Good
- Levels are chosen deliberately and consistently with the `otel` package: a business rule violation
  is `warn`, not `error`, and the doc says why.
- The doc comment enumerates all four outcomes and what each one logs.
- The nil-revision guard on the conflict branch is the right instinct — it just needs to extend to
  the `error` attr beside it.
