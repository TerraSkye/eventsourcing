# TODO: `eventbus/postgres/eventbus.go`

532 lines. PostgreSQL-backed `EventBus` — polling with LISTEN/NOTIFY wake-ups, persisted positions.

## Correctness
- [ ] **`lockPoolMaxConns = 4` silently caps concurrent subscribers.** `poll` holds a lock-pool
      connection for the whole call — including every `Handle` invocation for up to 100 events — so
      with five or more subscribers the fifth blocks waiting for a connection, and its events are
      delayed by another subscriber's slow handler. The constant is documented as bounding the pool
      but not as bounding *parallelism*. Either scale it with the subscriber count or make it an
      option, and say what it costs.
- [ ] **A metadata unmarshal error is silently discarded** (`:516-518`) — identical to
      `eventstore/postgres`. The event is delivered as if it had no metadata, so correlation and
      trace-propagation data vanishes without a signal.
- [ ] **`xmin::text::bigint` breaks at transaction-ID wraparound** (`:313`) — same as
      `eventstore/postgres/eventstore.go.md`. The intent is right and well explained; the mechanism
      needs 64-bit-safe handling.
- [ ] **`pgx.ErrNoRows` from the `FOR UPDATE SKIP LOCKED` query is always read as "another instance
      holds the lock"** (`:302-303`). It is *also* what happens when the subscription row has been
      deleted — in which case this subscriber silently polls forever, delivering nothing, with no
      error on the `Errors` channel. Distinguish the two.
- [ ] **`LIMIT 100` is hardcoded** (`:320`). Combined with the held transaction, batch size directly
      determines how long the row lock is held. Make it an option.

## Duplication
- [ ] **`scanEnvelope` is byte-identical to `eventstore/postgres/eventstore.go`'s** (`:489-532`) — 44
      lines duplicated across two packages, including the swallowed-error bug above, so a fix has to
      be applied twice. Extract to a shared internal package.

## Style
- [ ] **`slog.Warn` writes to the default logger from library code** (`:106`). The fallback it reports
      — sharing the main pool — "re-introduces the deadlock risk poll() is designed to avoid", so
      this is important enough to be an error the caller sees, not a line in someone's log. Return it
      from `NewEventBus`, or take a `*slog.Logger` option.
- [ ] **Error messages don't wrap sentinels** (`:135`, `:142`, `:145`), and the duplicate-subscriber
      message differs from the memory bus's for the same condition:
      `"subscriber %q already exists"` here vs `"handler with name %q already registered"` there.
      `errors.go` documents `ErrDuplicateHandler` as covering exactly this — wrap it in both.
- [ ] **`defer tx.Rollback(ctx)`** lacks the `//nolint:errcheck` that the sibling package's identical
      line carries.
- [ ] **Two goroutines per subscriber** (`:170`, `:179`) — the watcher can fold into `runSubscriber`'s
      defer, as in the memory bus.
- [ ] **The two `poll` error branches in `runSubscriber` are identical** (`:239-244`, `:246-251`).

## Good
- **`newLockPool` is a genuinely good piece of design, and the comment says why it exists**: `poll`
  holds its transaction across handler calls, and handlers need their own connections, so sharing one
  pool deadlocks. Most implementations discover this in production.
- **The MVCC snapshot filter** (`:274-279`) prevents the classic "poll by id > n" bug where a slow
  transaction's lower id commits after a higher one has already advanced the cursor, permanently
  skipping it. Explained clearly at the point of use.
- **`FOR UPDATE SKIP LOCKED` on the subscription row** gives multi-instance safety for free — only one
  process polls a given subscriber at a time — and `ON CONFLICT DO NOTHING` makes `ensureSubscription`
  idempotent.
- **The retry semantics are stated honestly** on the type: at-least-once, strict id order, and "a
  handler that keeps failing blocks that subscriber indefinitely". That last sentence is the one
  users need and the one most libraries omit.
- `listen` is correctly described as *purely an optimization* — polling still works if it never
  connects — and it reconnects on its own.
- `Listening()` exists specifically so tests can avoid a NOTIFY race, and says so.
