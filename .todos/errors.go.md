# TODO: `errors.go`

107 lines. Sentinel errors and three error types.

## Correctness
- [ ] **`StreamRevisionConflictError.Error()` panics when either revision is nil** (`errors.go:52-55`).
      `ToRawInt64()` is called on both `StreamState` fields unconditionally, and they are plain
      interfaces that default to nil. The KurrentDB store constructs exactly this shape — see the TODO
      at `eventstore/kurrentdb/eventstore.go:74`, which documents the panic as confirmed. Anything
      that logs or formats such an error crashes:
      ```
      err=... concurrency conflict: %!v(PANIC=Error method: runtime error: invalid memory address or nil pointer dereference)
      ```
      Fix here rather than at the call sites: render a nil revision as `"unknown"`. `[verified]`
- [ ] **`ErrDuplicateHandler` has a trailing space** (`errors.go:31`):
      `errors.New("duplicate handler registered ")`. It shows up in the panic message from
      `Register`. `[verified]`
- [ ] **`"business rule violation :%s"` has the space on the wrong side of the colon**
      (`errors.go:98`), and the phrase is *also* added by `command_handler.go:191`, so the real
      message reads `business rule violation: business rule violation :seat already taken`. Fix both
      halves together. `[verified]`
- [ ] **`ErrHandlerNotRegistered` dangles**: `"no handler registered for type"` with no type appended
      (`errors.go:26`). Either take the type or end the sentence.

## API
- [ ] **Error *types* are named with the `Err` prefix reserved for sentinel *values*.**
      `ErrSkippedEvent` and `ErrBusinessRuleViolation` are structs implementing `error`; the guide's
      convention is `XxxError` for those. `StreamRevisionConflictError` in the same file gets it
      right. Renaming to `SkippedEventError` / `BusinessRuleViolationError` is breaking — worth
      bundling into the next major.
- [ ] **All three error types use value receivers for `Error()` but are always constructed as
      pointers.** So both `T` and `*T` satisfy `error`, while every consumer matches on `*T`
      (`errors.As(err, &skipped)` with `*ErrSkippedEvent`). A value-constructed one is silently not
      matched — `errors_test.go:37` already builds one that way. Use pointer receivers so only one
      form implements `error`.
- [ ] **No `Is` or `As` methods on the error types**, so matching is pointer-identity only. Fine
      today; worth considering if any of these gain fields that shouldn't participate in equality.

## Good
- Every sentinel has a doc comment naming which component returns it and when — genuinely unusual and
  makes the whole package easier to consume.
- `NewBusinessRuleViolation` returning nil for a nil cause is the right nil-safety choice, and its doc
  explains the double-wrapping trap with `NewCommandHandler` explicitly.
- The unexported cause with `Cause()`/`Unwrap()` accessors is the correct shape.
