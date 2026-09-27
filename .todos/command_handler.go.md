# TODO: `command_handler.go`

356 lines. `NewCommandHandler` and its options.

**2 of this file's own tests fail on master, and both are genuine defects (items 1 and 2).**

## Correctness
- [ ] **A pinned `Revision(N)` decides against an empty aggregate and re-stamps versions from 1.**
      Failing test: `TestNewCommandHandler_PinnedRevisionSkipsStateFolding`.
      `revision` (`:155`) is initialised to `options.Revision` and then used as the **load starting
      point** (`:161`). With `WithStreamState(Revision(3))` on a stream that holds 3 events:
      ```
      LoadStreamFrom(Revision(3))  ->  0 events      (exclusive convention)
      decide sees state = 0                          (initial state, not the aggregate's)
      SAVE envelope Version = 1                      (collides with the existing version 1)
            against revision Revision(3)  -> accepted
      ```
      Two failures, one cause: business rules are evaluated against nothing, and `lastVersion` stays 0
      so new events overwrite existing version numbers.
      **Root cause:** `options.Revision` does double duty as the save-time assertion *and* the
      load-time start. Split into `loadFrom` and `saveAssert` — that also resolves item 2 and most of
      item 5. `[verified]`
- [ ] **Auto-converge on an empty stream saves with `Any{}`, disabling the concurrency check.**
      Failing test: `TestNewCommandHandler_AutoConvergeDefaultDoesNotPinEmptyStreamRevision`.
      `revision` only advances inside the fold loop (`:176`), which never runs for a new aggregate, so
      `saveRevision` is still `Any{}` at `:228`. Every store treats `Any{}` as "skip the concurrency
      check", so two commands racing to create the same aggregate **both succeed**. Contradicts this
      function's own doc ("saves against the revision it just loaded"). Should be `NoStream{}` or
      `Revision(0)` when nothing was loaded. `[verified]`
- [ ] **`NextExpectedVersion` is silently 0 when the conflict's revision isn't a `Revision`**
      (`:236`): `actual, _ := conflict.ActualRevision.(Revision)`. The discarded comma-ok yields 0 for
      any other type, nil included — which is exactly what the KurrentDB store produces (see the TODO
      at `eventstore/kurrentdb/eventstore.go:74`). A caller retrying against the reported version
      would assert "stream is empty". `[verified]`
- [ ] **`WithStreamState(nil)` stores a nil `StreamState`** (`:296`). `WithRetryStrategy` and
      `WithStreamNamer` both guard nil; this one doesn't. Postgres's `default:` branch then calls
      `ToRawInt64()` on a nil interface and panics; memory silently loads from offset 0 instead. Two
      different wrong behaviours from one unguarded option. `[verified]`
- [ ] **Retry re-folds incrementally, relying on a contract the interface never states.** `state`,
      `revision` and `lastVersion` live outside the retry closure (`:153-157`), so a retry loads only
      what is new and folds it into leftover state. Sound *only* because `LoadStreamFrom` is exclusive
      everywhere in this repo — which `eventstore.go` does not say. See `eventstore.go.md`. The doc at
      `:98` also mis-describes this as retrying "the whole load-evolve-decide-save cycle".
- [ ] **`time.Now()` is called per envelope** (`:216`), so events from one command get different
      `OccurredAt` values. Hoist one `now` above the loop.

## Style
- [ ] **The same 150-190 character error prefix appears six times** (`:170`, `:183`, `:191`, `:241`,
      `:249`, `:251`). Longest line is 188 chars. Extract
      `wrapErr(command, streamID, stage string, err error) error`.
- [ ] **`NewCommandHandler` is ~140 lines with a closure inside a closure.** Lift the retry body onto a
      small unexported struct holding `store`, `options`, `evolve`, `decide` — then items 1 and 5 can
      be reasoned about without tracking captured-variable lifetime across two closure boundaries.
- [ ] **Doc comments that no longer match the code:**
      - `handlerOptions.RetryStrategy` (`:269`) "If nil, no retries are performed" — never nil.
      - `handlerOptions.StreamNamer` (`:277`) "If nil, DefaultStreamNamer is used" — never nil.
      - `TestNewCommandHandler_NilRetryStrategyPanicsInsteadOfNoRetry`'s comment claims it panics; the
        guard at `:321` prevents it and the test passes.
      - `:191` adds "business rule violation" to a message `NewBusinessRuleViolation` already prefixes.
        See `errors.go.md`.
- [ ] **`DefaultStreamNamer` is mutable package-level state** (`:30`). Guide:
      [global state](https://google.github.io/styleguide/go/best-practices#global-state). Fully
      redundant with `WithStreamNamer`, and makes tests that override it non-hermetic. Its doc says to
      override "before any handler **runs**", but `:129` captures it when the handler is
      **constructed** — a later override silently does nothing.
- [ ] **Dead branch:** `if cfg == nil { return }` in `WithStreamState` (`:298`). `options` is always
      non-nil from `:125`; the other three options omit it.
- [ ] **Minor:** `var x = y` at `:151-155` where `:=` is preferred (`:157` is correctly `var`);
      banner comments (`// --- Evolve state ---`); `// Apply handler options` (`:124`) sits above code
      that *builds defaults*; `MetadataFuncs: []func(...){}` (`:128`) can be nil;
      `maps.Clone(baseMetadata)` (`:214`) allocates an empty map per event when no extractors are
      configured.

## Good
- The doc comments explain *why*, not just what — the auto-converge rationale at `:93-107` and
  `:135-144` is genuinely useful and rare.
- `%w` wrapping is consistent, so `errors.As` works end to end.
- `context` is threaded properly through load, decide and save.
- 21 test functions, several written specifically to pin down items 1 and 2. The test suite is ahead
  of the implementation.
