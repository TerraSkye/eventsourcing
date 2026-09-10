# TODO: `eventstore/memory/eventstore.go`

277 lines. In-memory `EventStore` for tests and local development.

## Correctness
- [ ] **Stored envelopes never get their `GlobalVersion` set — `LoadFromAll` always returns 0.**
      ```go
      ev := events[i]                                   // :154  copy taken BEFORE the assignment below
      m.events[streamId] = append(m.events[streamId], &ev)
      m.global = append(m.global, &ev)
      events[i].GlobalVersion = uint64(len(m.global))   // :157  writes to the CALLER's element, not ev
      ```
      Reproduced:
      ```
      caller's slice after Save: GlobalVersion = 1, 2
      LoadFromAll[0]: Version=1 GlobalVersion=0
      LoadFromAll[1]: Version=2 GlobalVersion=0
      ```
      `Envelope.GlobalVersion`'s documented purpose is "used to resume a global subscription from a
      specific point" — so any consumer resuming from it restarts from the beginning, forever, and
      re-processes the whole log. Set `ev.GlobalVersion` before storing. `[verified]`
- [ ] **Three different envelope identities in one loop.** `&ev` (a loop-local copy) goes into both
      `m.events` and `m.global`, while `&events[i]` (the caller's element) is what gets published to
      the bus (`:161`). So a subscriber and a later `LoadStream` see different values for the same
      event. Pick one copy and use it everywhere.
- [ ] **`Save` mutates the caller's slice** (`:157`), writing `GlobalVersion` into the argument. Same
      class of surprise as `otel/event_store.go`'s metadata rewrite. Either document it as part of the
      contract on `EventStore.Save` or stop doing it.
- [ ] **Iteration happens after the lock is released.** `LoadStreamFrom` captures the slice header
      under `RLock` and drops it at `:194`; the iterator closure then reads `events[offset]` unlocked.
      This is *safe* — a concurrent `append` writes at an index past the reader's captured `len` — but
      it is safe by a subtle argument that no comment records. Write it down before someone
      "simplifies" the capture.

## Style
- [ ] **The `tracer trace.Tracer` field is never used** (`:22`), and it drags an
      `go.opentelemetry.io/otel/trace` import into the in-memory store. Remove both.
- [ ] **`streamId` → `streamID`** throughout. Guide:
      [initialisms](https://google.github.io/styleguide/go/decisions#initialisms).
- [ ] **Malformed error strings:** trailing space in `"should exist: %w "` (`:135`); space before the
      colon in `"unsupported revision type for stream %s :%w"` (`:148`).
- [ ] **`NewMemoryStore(buffer int64)`** — a channel capacity is an `int`; the `int64` forces a
      conversion at the only use site.
- [ ] **The `len(events) == 0` early return drops `StreamID`** (`:105`), unlike every other return in
      the function.
- [ ] `errors.New("save events: event store is closed")` (`:101`) is the only error in the file not
      wrapping a sentinel — callers can't match it. Consider an `ErrStoreClosed`.

## Good
- **The comment at `:42-53` is excellent.** It explains why only `Revision`/`NoStream` carry a numeric
  offset, records the earlier bug where `Any{}`'s `-1` was read as a huge `uint64`, and states the
  half-open convention. That last point is the convention the whole repo depends on — see
  `eventstore.go.md`.
- `Close` being idempotent and turning subsequent `Save`s into errors matches the interface contract.
- `Events()` is honestly documented as non-interface, best-effort, and lossy under a slow consumer.
- Batch validation rejects a mixed-stream batch before mutating anything.
