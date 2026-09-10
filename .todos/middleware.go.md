# TODO: `middleware.go`

166 lines. The four middleware types, the two `Use` methods, and `wrapQueryHandler`.

## Correctness
- [ ] **Two unchecked type assertions in `wrapQueryHandler` panic on a caller's mistake.**
      - `qry.(T)` (`middleware.go:111`) — panics if a query of the wrong concrete type reaches the
        handler.
      - `result.(R)` (`middleware.go:130`) — panics if any middleware in the chain returns a
        differently-typed result, which is exactly what a short-circuiting middleware does.
      The comma-ok form is already used on the error path four lines above (`middleware.go:120`);
      apply it on both and return a descriptive error instead. Guide:
      [don't panic](https://google.github.io/styleguide/go/best-practices#dont-panic).

## Style
- [ ] **Both `Use` methods loop to append** (`middleware.go:47-49`, `98-100`).
      `b.middlewares = append(b.middlewares, middlewares...)` is one line and does the same thing.
- [ ] **`CommandBus.Use` and `QueryBus.Use` live away from their types.** Methods on `*CommandBus` are
      split across `command_bus.go` and here; same for `*QueryBus`. Keeping a type's methods with the
      type is the usual organisation and makes the bus files self-contained. Counter-argument: the
      current grouping keeps all middleware concepts in one place — worth a deliberate decision
      either way rather than drift.

## Documentation
- [ ] **The "Use before Register" constraint is documented in four places** (here twice, plus
      `CommandBus.Register` and `EventBus.Use`) and enforced nowhere. It is a deliberate design
      choice — the chain is baked in at registration — so consider making a post-registration `Use`
      call fail loudly rather than silently no-op on already-registered handlers.

## Good
- Every middleware type carries a complete, compilable usage example — these are the docs a new user
  actually needs.
- The ordering guarantee ("first passed to Use is outermost and runs first") is stated consistently on
  all four types.
- `wrapQueryHandler` returning `h` unchanged when there are no middlewares avoids a pointless
  allocation and indirection per query.
