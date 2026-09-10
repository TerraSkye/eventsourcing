# TODO: `eventstore/kurrentdb/eventstore.go`

462 lines. KurrentDB-backed `EventStore`.

**This is the least finished implementation in the repo — it carries nine `TODO` markers, several of
which document confirmed data-correctness bugs rather than nice-to-haves.** They are transcribed
here so they can be scheduled rather than read only by whoever opens the file.

## Correctness — errors are misreported as success
- [ ] **Every `Recv` error during iteration is reported as `io.EOF`** — in `LoadStream` (`:239`),
      `LoadStreamFrom` (`:288`) and `LoadFromAll` (`:331`), each with its own TODO saying so. A
      mid-stream connection drop is indistinguishable from a clean end of stream, so an aggregate
      rebuilds from a **truncated** history and the command handler proceeds as if it had the whole
      thing. This is the most dangerous bug class in the repo: silent partial reads that look
      successful. Distinguish the library's end-of-stream sentinel from a transport failure and
      surface the latter through `Iterator.Err()`.
- [ ] **Any error opening a read is reported as `ErrStreamNotFound`** (`:220`, TODO at `:205`),
      including genuine connectivity failures. A caller retrying "stream not found" as a normal empty
      stream will happily create a duplicate aggregate during an outage.
- [ ] **`LoadFromAll` accepts `version` and never uses it** (TODO at `:371`, `//TODO fix 'from'` at
      `:376`) — every call reads from the very beginning. Any consumer resuming a global subscription
      re-processes the entire log on every restart. Either implement the position or return
      `ErrInvalidRevision` rather than silently ignoring the argument.
- [ ] **Revision conflicts lose both revisions and panic when formatted** (TODO at `:74` and `:172`).
      `ExpectedRevision` and `ActualRevision` are left nil, and
      `StreamRevisionConflictError.Error()` calls `ToRawInt64()` on both — *"confirmed"* per the
      file's own comment. Consequences reach outward:
      - anything logging the error crashes (`%!v(PANIC=Error method: ...)`),
      - `command_handler.go:236` decodes `NextExpectedVersion` as 0 for a stream of any length.
      Fix the nil-safety in `errors.go` **and** populate the fields here. `[verified]`
- [ ] **`revision` is accepted by `Save` and never applied** (`//todo use the revision here`, `:167`).
      The caller's concurrency expectation is silently dropped, so `NoStream{}` and `Revision(N)` do
      not protect anything on this store.

## Style
- [ ] **`fmt.Printf` in the retry notify callback** (`:165`). A library must not write to stdout —
      and the format string has no trailing newline, so retries run together. Take a `*slog.Logger`
      as an `Option`, defaulting to a no-op.
- [ ] **`//TODO enhance error variants`** (`:220`) — same site as the `ErrStreamNotFound` bug above.
- [ ] **Value receivers on `eventstore`** while the other stores use pointers. Pick one per repo.
- [ ] `NewEventStore` returns the interface rather than the concrete type — same note as the postgres
      store.

## Good
- **The TODOs are exemplary.** Each one states the symptom, the mechanism, and the user-visible
  consequence — `:74-80` even records that the panic was *confirmed*. Whoever wrote these did the
  diagnostic work; only the fixing is left. That is why this file's problems are all listed above
  rather than discovered.
- `WithBackoff` / `defaultSaveBackoff` is a clean option, and `isRetryableError` correctly separates
  transient failures (using `backoff.Permanent` for the rest) so leader elections don't surface as
  errors.
- The `Save` doc explains the 30-second default retry window and what `NextExpectedVersion` means.

## Recommendation
Until the read-path items are fixed, this store should not be used where a truncated stream matters.
Consider a package-level doc note saying so, so the state is visible on pkg.go.dev rather than only
in the source.
