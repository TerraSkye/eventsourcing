# TODO: `eventstore/postgres/eventstore.go`

339 lines. PostgreSQL-backed `EventStore` on pgx/v5.

## Correctness
- [ ] **An abandoned iterator leaks a pooled connection.** `queryRows` (`:271-292`) closes `rows` only
      when iteration reaches the end or errors. A consumer that stops early — which
      `command_handler.go:181` does on `iter.Err()` — leaves a pgx `Rows` open, holding a connection
      out of the pool until finalisation. This is the concrete cost of `Iterator` having no `Close`
      (see `iter.go.md`); postgres is where it actually hurts.
- [ ] **A corrupt metadata column is silently discarded** (`:322-328`):
      ```go
      if err := json.Unmarshal(metadata, &meta); err != nil {
          meta = make(map[string]any)   // error dropped
      }
      ```
      The event is returned as if its metadata were simply empty — so correlation IDs and trace
      propagation vanish with no signal. Guide: don't discard errors. Return it, or at minimum
      surface it.
- [ ] **`version.ToRawInt64()` on a nil `StreamState` panics** (`:234`, `:252`). Reachable via
      `WithStreamState(nil)` — see `command_handler.go.md`. The `memory` store's `default:` silently
      reads from the beginning instead, so the same call has two different wrong behaviours. Guard
      here and fix the option at source. `[verified]`
- [ ] **`xmin::text::bigint` breaks at transaction-ID wraparound** (`:257`). `xmin` is a 32-bit
      counter that wraps; comparing it as a bigint against `pg_snapshot_xmin` will misbehave after
      wraparound on a long-lived database. The intent — don't skip past in-flight transactions — is
      right and well documented; the mechanism needs `txid_current`-style 64-bit handling or an
      explicit epoch.
- [ ] **The unique-violation path reports the caller's `StreamState` as `ExpectedRevision`** (`:154`),
      which may be `Any{}`. Formatting that conflict then renders "expected version -1". Use the
      `Revision(currentVersion)` that was actually asserted, or leave it unset.
- [ ] **`currentVersion` from `MAX(stream_position)`** (`:78`) assumes positions are contiguous from 1.
      A stream with a gap (or with position 0) makes the `NoStream`/`StreamExists` checks wrong.
      `COUNT(*)` matches the semantics the doc describes ("the stream's current length").

## API
- [ ] **`NewEventStore` returns the interface, not the concrete type** (`:34`). Guide:
      [return concrete types](https://google.github.io/styleguide/go/best-practices#returning-concrete-types).
      The type is unexported so there is no choice today — exporting it would let callers reach
      store-specific helpers without a type assertion.
- [ ] **The required schema is documented nowhere.** `NewEventStore` says it "does not create or
      migrate the schema" without saying what schema. Put the expected DDL — columns, the unique
      constraint on `(stream_id, stream_position)` that the conflict detection depends on, and the
      indexes the queries need — in the package doc.

## Style
- [ ] **The same SELECT column list is repeated four times** (`:189`, `:224`, `:229`, `:236`). One
      `const selectEnvelopeColumns` removes the risk of them drifting apart from `scanEnvelope`'s
      argument order.
- [ ] **`LoadStreamFrom`'s `StreamExists` and `Any` branches are byte-identical** (`:223-230`) after
      their differing precondition checks. Fall through instead.
- [ ] **`len(events) == 0` returns `Successful: true` with no `StreamID`** (`:52`) — same drop as the
      memory store.

## Good
- **The advisory lock plus transaction is the right concurrency design**, and the doc explains that
  conflicts are caught two ways (explicit check and unique violation) and that nothing is persisted
  on either.
- **`LoadFromAll`'s snapshot condition is genuinely thoughtful** — the doc explains the skipped-row
  hazard it prevents, which is the failure mode most hand-rolled "poll by id > n" implementations
  have and never notice.
- `defer tx.Rollback(ctx)` with the `//nolint:errcheck` marker is the correct idiom.
- Defensive defaults on write — `uuid.New()` for a nil event ID, `time.Now()` for a zero timestamp,
  `{}` for nil metadata — mean a partly-filled envelope still round-trips.
- Errors consistently wrap sentinels with `%w` and name the stream.
