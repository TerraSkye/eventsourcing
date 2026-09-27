# TODO: `eventbus/kurrentdb/eventbus.go`

445 lines. KurrentDB persistent-subscription-backed `EventBus`.

## Correctness
- [ ] **A subscriber dies permanently after 100 reconnects.** `backoff.WithMaxRetries(bo, 100)`
      (`:193`); when it is exhausted, `runSubscriber` reports "max retries exceeded" on the `errs`
      channel — **through the drop-if-full select** (`:212-215`) — and returns. So during a long
      outage a subscription can stop forever, and the one notification of that fact can itself be
      silently dropped. Given `MaxElapsedTime = 0` ("never stop due to elapsed time") the retry cap
      looks unintentional. Retry indefinitely, or surface the give-up somewhere that cannot be
      dropped.
- [ ] **`stream.Recv()`'s result is dereferenced before any nil check** (`:238-240`):
      ```go
      subscriptionEvent := stream.Recv()
      kEvent := subscriptionEvent.EventAppeared      // deref
      if subscriptionEvent.SubscriptionDropped != nil { ... }
      ```
      If `Recv` can ever return nil on a closed or failed stream, this panics inside the subscriber
      goroutine. Check the result before touching its fields, and reorder so the dropped-subscription
      test comes first.
- [ ] **A failing handler leaves the event unacknowledged and moves on** (`:302-308`). The comment
      says it is "left unacknowledged for redelivery", but the loop continues and acks event N+1 — so
      redelivery arrives out of order relative to events already processed. The client's explicit
      `Nack` is not used; using it would make the intent unambiguous and the redelivery prompt.
- [ ] **`buffer` is dead configuration** — the file's own TODO at `:48-51`: "stored but never read
      anywhere in this package ... misleads callers into thinking it affects behavior". Wire it up or
      remove it from `NewEventBus`'s signature.
- [ ] **`EnsurePersistentSubscription` swallows every non-not-found error** — TODO at `:146-152`. A
      network failure returns nil, `Subscribe` proceeds as though the subscription were ready, and the
      real failure surfaces later, if at all, as an async stream error.

## Style
- [ ] **The drop-if-full error send is written inline five times** (`:199`, `:212`, `:280`, `:304`,
      `:311`). The memory, file and postgres buses all have a `sendErr` helper; add one here.
- [ ] **`errors.New("subscription dropped, reconnecting")`** (`:242`) is control flow dressed as an
      error, and it is delivered to `Errors()` on every routine reconnect — so consumers see an error
      for a non-error. Use a package-level sentinel and skip reporting it.
- [ ] **Stale message** `"filter and handler cannot be nil"` (`:85`) — only `handler` is checked. The
      identical stale string is in `eventbus/memory/eventbus.go:83`.
- [ ] **No sentinel wrapping** on the three `errors.New`/`fmt.Errorf` returns from `Subscribe`.
- [ ] **Four options panic on a type mismatch** (`:365`, `:379`, `:404`, `:435`) — the
      `SubscriberOption func(cfg any)` weakness; see `event_bus.go.md`.
- [ ] `// or use kEvent.ID if available` (`:288`) — leftover uncertainty; resolve it.

## Good
- Exponential backoff with `MaxElapsedTime = 0` and `backoff.WithContext` means reconnection is both
  patient and promptly cancellable — the right combination for a long-lived subscription.
- System events (checkpoints, catch-ups) are correctly recognised and skipped rather than treated as
  malformed.
- `EnsurePersistentSubscription` being idempotent — leaving an existing subscription untouched — is
  the right default for a startup call.
- `ErrSkippedEvent` acked as success, consistent across every bus in the repo.
- Like the kurrentdb event store, the known gaps are written down as TODOs with their consequences
  spelled out.
