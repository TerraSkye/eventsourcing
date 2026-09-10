# TODO: `query_bus.go`

169 lines. `QueryBus`, registration, and `Validate`.

## API
- [ ] **`RegisterQueryHandlerFunc` takes the unexported type `queryHandlerFunc[T, R]`** (`:96`). It
      compiles for callers because a func literal converts implicitly, but pkg.go.dev renders an
      exported function whose parameter type nobody outside the package can name or reference. Take
      `func(ctx context.Context, qry T) (R, error)` directly and convert inside.
- [ ] **`panic(ErrDuplicateHandler)` carries no context** (`:116`). Every other duplicate-registration
      panic in the repo names the offending type (`command_bus.go:255`, `event_handler.go:121`).
      Include the key: `panic(fmt.Errorf("query handler already registered for %s: %w", key, ErrDuplicateHandler))`.
- [ ] **Registration panics rather than returning an error** — same call as `CommandBus.Register`;
      whatever is decided there should apply here for consistency.

## Style
- [ ] **`errs := make([]error, 0)`** (`:139`) — `var errs []error`; `append` and `errors.Join` both
      handle nil, and `len(errs) > 0` still works.
- [ ] **`Validate`'s error message says "unknown query handler"** (`:142`) for what is actually a
      *missing* handler for a known requestee. "no handler registered for query %s" matches the
      sentinel vocabulary used elsewhere.
- [ ] **`Validate` is not wired to anything.** It only reports pairs that went through
      `NewQueryGateway`; nothing calls it, and nothing in the docs says where it belongs in a startup
      sequence beyond "call it during startup". A short example in the doc comment would land it.

## Good
- **`queryKey`'s doc comment (`:74-81`) is a model of its kind** — it explains the `(*T)(nil)` trick,
  why the obvious `*new(R)` is wrong, and why `reflect.TypeOf` isn't needed. Anyone tempted to
  "simplify" it back is stopped by the comment. This is the fix from commit 12b5592 and the pattern
  `command_bus.go` still needs.
- The requestee/`Validate` mechanism turns a class of runtime "no handler" errors into a startup
  check. Good design, under-advertised.
- `handlerFor` holds the read lock only across the map lookup, with a comment saying why.
- `WithQueryTimeout`'s doc is precise about the ceiling-not-override semantics and about what a zero
  value means.
