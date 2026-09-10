# TODO: `logging/query_handler.go`

62 lines. `queryHandlerLogger`, `WithQueryLogging`, and the `QueryLogging` middleware.

## Style
- [ ] **Same eager-attribute cost** as the other two files in this package: `q.logger.With(...)` plus
      `fmt.Sprintf("%T", qry)` on every query regardless of level. See `logging/command_handler.go.md`
      for the measurements and the fix.
- [ ] **`string(qry.ID())` allocates and copies on every query** (`:25`). Tracked at source in
      `query_handler.go.md` — `Query.ID()` should return a `string`.
- [ ] **Key naming**: `query`, `queryID` (lowerCamel) — consistent with `command_handler.go` but not
      with `event_handler.go`. Resolve package-wide.
- [ ] **Message capitalisation is inconsistent across the package**: `"Query"`, `"Query succeeded"`
      here versus `"event processing started"` in `event_handler.go`. Also `"Query"` alone, as the
      before-message, carries no verb.
- [ ] **A nil logger panics** — same as the command variant.

## Structure
- [ ] **This is the only one of the three implemented as a struct** rather than a closure.
      `WithQueryLogging` returns `*queryHandlerLogger` and `QueryLogging` immediately unwraps it back
      to a method value (`:60`). Either shape is fine, but the package should pick one.

## Good
- The struct form keeps `HandleQuery` readable and gives the doc comment a natural home on the method.
- Correctly threads `ctx` into every log call via the `*Context` variants.
