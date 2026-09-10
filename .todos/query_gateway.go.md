# TODO: `query_gateway.go`

114 lines. `QueryGateway` — the typed callable facade over a `QueryBus`.

## Correctness
- [ ] **Re-panicking loses the original stack** (`:106`). `panic(o.panic)` raises the value again on
      the caller's goroutine, but the stack now starts here, not where the handler actually panicked.
      Capture `debug.Stack()` in the recover at `:94` and include it in the re-panicked value.
      (Same gap as `command_bus.go`'s recover.)
- [ ] **An abandoned handler goroutine leaks until it returns** (`:88-101`). Documented and
      unavoidable — Go cannot interrupt a goroutine — but it pins `qry`, the result, and everything
      they reference for as long as the handler runs. Worth saying in the doc that a handler which
      ignores `ctx` leaks proportionally to its own runtime, so callers know the cost of
      `WithQueryTimeout` on a badly behaved handler.

## Style / performance
- [ ] **A goroutine and channel per query whenever `ctx` is cancellable** (`:86-101`). The
      `ctx.Done() == nil` fast path (`:71`) only helps `context.Background()`, which is the uncommon
      case in a server. For a fast in-memory read model this doubles the cost of the query. Consider
      running inline when no timeout is configured *and* letting the caller opt into the
      cancellation wrapper, or document the trade-off so users know cancellation is not free.
- [ ] **The `outcome` struct is declared inside the returned closure** (`:81-85`), so it is redeclared
      conceptually on every call. Hoist it to package scope as an unexported generic type.

## Good
- **The comments here are the best concurrency documentation in the repo.** `:69-73`, `:75-80` and
  `:89-92` each explain a decision that would otherwise look arbitrary: why the fast path exists, why
  the channel is buffered, and why the panic is re-raised on the caller's goroutine. The buffered
  channel in particular is load-bearing — an unbuffered one would leak a goroutine per abandoned
  query, forever.
- Honest about the limitation instead of hiding it: "The handler is not interrupted — Go cannot do
  that — so one that ignores ctx runs to completion off to the side".
- `GenericQueryGateway` kept as a deprecated type alias, with the `Deprecated:` marker godoc and
  tooling understand.
- Error messages use `%T` on `(*R)(nil)`, consistent with `queryKey` — the interface-result fix is
  applied here too.
