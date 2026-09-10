# TODO: `event.go`

39 lines. The `Event` interface and `Envelope`.

## API
- [ ] **`Envelope.Metadata map[string]any` is shared mutable state with no ownership rule.** Nothing
      says whether the producer, the store, or the consumer owns it after `Save`.
      `otel.TelemetryStore.Save` copies it and then writes the copy back into the caller's
      `events[i]` (see `otel/event_store.go.md`); `NewCommandHandler` clones it per envelope. Document
      who may mutate it and when, or make it an accessor over an unexported map.
- [ ] **`Version` and `GlobalVersion` are documented but not enforced.** "starting at 1" for `Version`
      is load-bearing for `command_handler.go`'s `lastVersion` arithmetic and for the exclusive
      `LoadStreamFrom` convention (see `eventstore.go.md`), but nothing validates it on `Save`.

## Good
- **`EventType() string` — the type names itself.** This is the reflect-free design that
  `command.go` should copy; it survives renames, gives a stable wire name, and makes the event
  registry possible.
- `Envelope`'s field comments each say what the value is *for*, not just what it is —
  `GlobalVersion`'s "used to resume a global subscription from a specific point" is exactly the
  context a reader needs.
