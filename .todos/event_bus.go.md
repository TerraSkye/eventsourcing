# TODO: `event_bus.go`

33 lines. The `EventBus` interface and `SubscriberOption`.

## API
- [ ] **`SubscriberOption func(cfg any)` throws away all type safety.** An option built for the memory
      bus and passed to the postgres bus either silently does nothing or panics inside a type
      assertion, and nothing catches it at compile time. The guide's advice against `any` applies
      directly. Options:
      - a `SubscriberOption` interface with an unexported `apply` method per implementation (the
        pattern `otel/config.go` already uses correctly), or
      - a concrete shared `SubscriberConfig` struct that all buses read, with
        implementation-specific fields ignored by those that don't support them.
- [ ] **`Use` must precede `Subscribe`, enforced only by documentation.** Same startup-only
      constraint as `CommandBus.Use`. A `Subscribe` after the first middleware-consuming call could
      return an error rather than silently under-wrapping.

## Style
- [ ] **`Errors() <-chan error` has no documented lifecycle.** Is it closed on `Close`? Buffered? Does
      a slow consumer block delivery? An unread channel that drops or blocks is the usual failure
      mode; say which happens.

## Good
- Small, focused interface — four methods, each doing one thing.
- `Subscribe` documents its three distinct error conditions explicitly.
