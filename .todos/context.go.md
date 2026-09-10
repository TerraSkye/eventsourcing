# TODO: `context.go`

138 lines. Envelope/causation values carried on `context.Context`.

## Correctness
- [ ] **`WithEnvelope` dereferences `env` without a nil check** (`context.go:28`). The doc covers
      `env.Event == nil` but not `env == nil`, which panics. Guard or document.

## Style / performance
- [ ] **Seven chained `context.WithValue` calls per envelope** (`context.go:34-40`). Each one
      allocates a new `valueCtx` node, so every event carries a 7-deep linked list, and every
      `*FromContext` lookup walks it comparing keys. Store one unexported struct under one key and
      have the eight accessors read fields off it: 1 allocation instead of 7, O(1) lookup instead of
      O(depth). This is on the per-event hot path.
- [ ] **Eight copies of the same six-line accessor** (`context.go:46-119`, `131-137`). Collapse to one
      generic helper:
      ```go
      func valueFrom[T any](ctx context.Context, key ctxKey) T {
          v, _ := ctx.Value(key).(T)   // zero value when absent or wrong type
          return v
      }
      ```
- [ ] **The `v != nil` guard before each type assertion is redundant.** A type assertion against a nil
      interface already fails through the comma-ok form.
- [ ] **`globalVersionKey ctxKey = "global_version"`** is snake_case while its seven siblings are
      camelCase (`context.go:17`). Unexported so it is cosmetic, but pick one.

## Good
- `ctxKey` is an unexported named type, so these keys cannot collide with another package's — the
  central rule for context keys, correctly applied.
- Every accessor documents its zero value on a miss, and the type assertions are all comma-ok, so a
  wrong-typed value degrades instead of panicking.
- `WithCausation` is kept deliberately separate from `WithEnvelope`, and the doc says why.
