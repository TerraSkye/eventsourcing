# TODO: `eventbus/memory/eventbus.go`

262 lines. In-process `EventBus` — one buffered channel and worker goroutine per subscriber.

## Correctness
- [ ] **`Close` silently discards every buffered event.** `runSubscriber`'s select (`:181-194`) has
      `<-ctx.Done()` and `<-s.events` as equal cases; once `Close` cancels, Go picks between them at
      random, and the first time it picks `Done` the worker returns — abandoning everything still
      buffered. So a `Close` during normal traffic drops events that were already accepted by
      `Dispatch` and reported to the producer as delivered.
      `CommandBus.worker` solves exactly this correctly, with a two-stage drain: exit only when the
      cancel signal fires **and** a non-blocking receive finds the queue empty. Copy that shape.
- [ ] **`Subscribe` doesn't wrap `ErrDuplicateHandler`** (`:94`). `errors.go:27-30` documents that
      sentinel as being returned by "[EventBus.Subscribe] implementations" — this implementation
      returns a bare `fmt.Errorf` instead, so `errors.Is` fails and the documented contract is
      unmet. Same for `"eventbus is closed"` (`:90`), which has no sentinel at all.
- [ ] **Stale error message** (`:83`): "filter and handler cannot be nil" — only `handler` is checked,
      and `filter` isn't a parameter.

## API
- [ ] **`Dispatch` is not part of `cqrs.EventBus`** (`:221`), so the only way to feed this bus is to
      hold the concrete `*EventBus`. That is a reasonable design, but nothing in the interface docs
      says how events get *in*, and `event_bus.go` explicitly punts ("How events are fed into the bus
      ... is left to the implementation"). Worth an example in the package doc.
- [ ] **`WithFilterEvents` panics on a type mismatch** (`:257`) — the direct consequence of
      `SubscriberOption func(cfg any)`. Passing this bus's option to the postgres bus is a runtime
      panic that no compiler catches. Tracked at source in `event_bus.go.md`.

## Style
- [ ] **Two goroutines per subscriber** (`:123`, `:132`). The second exists only to await
      `workerCtx.Done()` and call `removeSubscriber`; that can be a `defer` inside `runSubscriber`,
      halving the goroutine count and removing a `wg.Add`.
- [ ] `filter: &filter{events: make([]string, 0)}` (`:107-109`) — a nil slice behaves identically for
      `len` and `slices.Contains`.

## Good
- **The comments explain the two hard decisions and both are correct:**
  - `:235-240` — why the blocking send must happen outside `b.mu` (a full buffer would otherwise block
    `Close` and every other `Dispatch`), and why the send is bounded by `s.ctx.Done()` so a removed
    subscriber doesn't strand the sender forever.
  - `:125-130` — why the worker watches a *child* context rather than the caller's, so a subscriber
    created with `context.Background()` still shuts down instead of leaking.
- `events` is deliberately never closed, and the `subscriber` doc says so — which is what makes the
  lock-free send in `Dispatch` safe.
- `Close` cancels, then waits on the `WaitGroup`, then closes `errs` — the right order, so no worker
  can send on a closed channel.
- Per-subscriber back-pressure (`Dispatch` blocks) rather than silent dropping, and the doc states the
  trade-off plainly.
