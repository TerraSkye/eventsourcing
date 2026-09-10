# TODO: `event_handler.go`

162 lines. `EventHandler`, `OnEvent`, and `EventGroupProcessor`.

**3 of this file's tests fail on master**, each with a `.bug/` write-up already filed. All three are
the same underlying problem: `StreamFilter` reconstructs a type from a zero value.

## Correctness — `StreamFilter`
- [ ] **Value-type handlers miss every registry alias.** Failing test:
      `TestEventGroupProcessor_StreamFilter_ValueHandlerMissesPointerRegisteredAliases`
      (`.bug/streamfilter-value-handler-misses-pointer-registered-aliases.md`).
      `RegisterEvent(&CartCreated{})` keys `typeToNames` under the **pointer** form
      `"*eventsourcing.CartCreated"`, because `RegisterEventByType`'s factory always returns a
      pointer. But `OnEvent(func(ctx, ev CartCreated) error)` — equally legal when the event's
      methods are value-receiver — has `EventInstance()` return a bare `CartCreated{}`, whose `%T` is
      `"eventsourcing.CartCreated"`. No asterisk, no match, and every alias added via
      `RegisterEventByName` is silently dropped from the subscription filter. `[verified]`
- [ ] **A pointer handler over a value-receiver event panics.** Failing test:
      `TestStreamFilter_PointerHandlerOfValueReceiverEventPanics`
      (`.bug/streamfilter-nil-pointer-dereference-on-fallback.md`).
      `EventInstance()` returns `var zero T`, which for `T = *CartCreated` is a **nil pointer**;
      calling a value-receiver method on it dereferences nil. `[verified]`
- [ ] **A handler without `EventInstance` is silently dropped.** Failing test:
      `TestEventGroupProcessor_StreamFilter_HandlerWithoutEventInstanceIsDropped`
      (`.bug/streamfilter-drops-handler-without-eventinstance.md`).
      `NewEventGroupProcessor` only requires `EventName() string` (`:114`), but `StreamFilter` only
      considers handlers that also satisfy `EventInstance() Event` (`:156`). A hand-rolled handler
      meeting the enforced contract registers fine, routes fine, and is absent from the filter —
      meaning the bus never subscribes to the events it handles. `[verified]`
- [ ] **Root cause and fix.** `EventInstance()` returning a zero value cannot represent a type
      faithfully — `var zero T` is nil for pointers and loses the pointer-ness for values. Key the
      routing and the filter on `reflect.Type` (or on the `EventName()` string that is *already*
      required and already correct), and drop `EventInstance` entirely. That closes all three at once.
- [ ] **The doc comment now describes the bug as intended behaviour.** `:150-152` says an unregistered
      type "contributes no name and is silently omitted", while the tests quote an earlier doc
      promising it is "never silently dropped" and reference issue #55 as having fixed exactly that.
      Decide which contract is right and make doc, code and tests agree.
- [ ] **`StreamFilter` can return duplicates.** Two handlers whose types register under a shared alias
      both append it; nothing dedupes before `sort.Strings` (`:154-160`).

## Correctness — routing
- [ ] **`Handle` keys on `fmt.Sprintf("%T", ev)`** (`:134`), so it inherits the package-name collision
      described in `command_bus.go.md`: two event types in differently-pathed packages sharing a
      package name route to the same handler.

## API
- [ ] **`NewEventGroupProcessor` panics on a structural type-assertion failure** (`:116`). The
      requirement — "must implement an internal `EventName() string`" — is invisible to the compiler,
      so a wrong handler is a runtime panic rather than a build error. Export a small
      `NamedEventHandler` interface and take that, and the check becomes compile-time.
- [ ] **`OnEvent` returns `EventHandler`, hiding the `EventName`/`EventInstance` methods it relies on.**
      Callers can't see why an `OnEvent` handler works with `EventGroupProcessor` and a
      `NewEventHandlerFunc` one doesn't. Returning the narrower named interface documents itself.

## Style
- [ ] **`sort.Strings` → `slices.Sort`** (`:160`). Module targets Go 1.25.
- [ ] `NewEventHandlerFunc`'s doc spends four lines explaining what it *cannot* do
      (`EventGroupProcessor`) — a sign the two handler kinds want distinct types rather than one
      interface with an unwritten side contract.

## Good
- The doc on `NewEventHandlerFunc` warns about the `EventGroupProcessor` incompatibility *before* the
  user hits it, and points at `OnEvent` as the fix.
- `typedEventHandler.Handle` returning `ErrSkippedEvent` rather than an error for a non-matching type
  is the right model, and the logging and otel layers both honour it.
- Every failing behaviour above already has a test and a `.bug/` file. The diagnosis work is done.
