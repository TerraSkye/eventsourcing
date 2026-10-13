# Handoff: projections (branch `feature/projections`)

Status as of 2026-10-13. This document is for whoever picks the branch up
next. Delete it when the branch is merged.

## Where things stand

| Area | State |
|---|---|
| `GlobalReader` (`ReadAll`/`Head`) on memory, file, postgres, kurrentdb | done; `EventStore.LoadFromAll` removed |
| `eventsourcingtest.GlobalReaderAcceptanceTest` | done; runs against all four stores |
| `projection.Runner`: one loop for single-segment and segmented projections | done |
| `projection/memory` and `projection/postgres` stores | done |
| `projectiontest.StoreAcceptanceTest` | done; runs against both stores |
| User docs: `docs/how-to/cached-projection.md` | done |
| `automation` package | **design draft only**: signatures and docs; `Run` panics |

`go test -race -tags=integration ./...` passes; it needs Docker for
testcontainers (Postgres, KurrentDB). CI runs exactly that command.

## How a projection runs

Read `projection/doc.go` first, then `projection/processor.go` (the
`runSegment` loop) and `projection/store.go` (the `Store` contract).

A projection is a `projection.Projection` value: a `Name`, one `Handler`
(`eventsourcing.EventHandler`) and the `Events` (registered names) it
handles, plus optional `Version`/`Reset`, `Segments`/`SegmentKey`,
`OnError` and tuning. `NewRunner(p, reader, store)` validates it and
panics on wiring mistakes, including unregistered event names.

Every segment, and a single-segment projection is just segment 0, runs the
same loop: `Store.Claim` the segment, `ReadAll` after its checkpoint
filtered by `Events`, run the handler for the batch inside `Store.Commit`
(read-model writes and checkpoint in one unit of work, fenced on the
claim), and at the head wait for `Nudge` or `PollInterval`. Instances that
do not hold a claim report `Standby` and answer `Live`/`WaitUntil` from the
status the owner publishes.

A failed batch is retried event by event to pin the failing event; then it
stalls (default) or skips per `OnError`, recording a dead letter when the
store implements `DeadLetterStore`. Errors back off with
`cenkalti/backoff` (0.5s to 30s, jittered, never stops). `Version` +
`Reset` rebuild on start; `Runner.Rebuild` rebuilds on demand.
`IsReplaying(ctx)` is true for events up to the segment's `ReplayUntil`.

## Decisions made on this branch

- **Polling only.** The `Notifier` interface on `GlobalReader` and
  `NudgeOnSave` were removed: they only reduced latency, and only the
  memory store implemented `Notifier`. Runners poll; `Runner.Nudge` stays
  (automations use it through `Runner.Watch`). Add a wake-up signal back
  when a store can offer one across processes (Postgres `LISTEN/NOTIFY`),
  preferably as an explicit runner option rather than a type assertion.
- **One handler plus event names**, the same shape as
  `EventBus.Subscribe` with `WithFilterEvents`. The `Router` interface was
  removed, so a switch-style handler works as well as an
  `EventGroupProcessor` (pass `group.StreamFilter()` as `Events`).
- **No logger in the runner.** Logging is done by wrapping the handler
  (`logging.WithEventLogging`), like everywhere else in the module.
- **Stores live in subpackages** (`projection/memory`, `projection/postgres`),
  mirroring `eventstore/` and `eventbus/`. The `projection` tests are an
  external test package so they can import `projection/memory`.
- **Postgres appends are serialised.** The global read could otherwise skip
  an event forever (ids 1 and 3 visible while 2's transaction was open).
  `eventstore/postgres` takes one advisory lock in every `Save`, held until
  commit, so ids commit in order and `ReadAll` is a plain `id > $after`.
  Cost: appends to different streams no longer commit in parallel.
  `TestReadAll_AppendsBecomeVisibleInIDOrder` pins the guarantee.
- **`SaveStatus` publishes phase, head and error only.** Position and
  Version always come from the checkpoint, Owner from the claim. The
  acceptance suite caught the memory store deviating; it now matches
  Postgres.

- **Postgres heartbeats use `clock_timestamp()`**, not `now()` (which is
  the transaction's start). `Commit` holds the segment row locked for the
  whole batch, so a competing `Claim` waits for it; with `now()` a batch
  longer than `ClaimTTL` left an already-expired heartbeat and was taken
  over right after committing. `TestCommit_BatchLongerThanTTLKeepsClaim`
  pins it. The memory store does not lock during a batch; documented.

## Known issues

- **`TestRunner_Rebuild` is flaky** (about 1 in 5 under `-race`). Real bug:
  `processor.go` sends `req.done` right after `Store.Reset`, before the
  runner updates its in-memory status, so `Rebuild` returns while the
  status still shows the old position and an immediate `WaitUntil` returns
  before anything was replayed. Fix: reload and publish the reset status
  before signalling `done`.
- `handleFailedBatch` has an unused `firstFailure` parameter and a stray
  `_ = handlerErr`.
- **Status after a clean shutdown.** A stopped runner releases its claim
  but leaves its last phase (often `live`) in the store; standby instances
  believe it until another instance claims the segment.
- **Memory store** cannot roll back a handler's writes, so delivery is
  at-least-once after a handler failure. Postgres is exactly-once.

## Not done yet

- **Automations** (`automation/`): implement `Automation.Run` against the
  design in `automation/doc.go` (claim with `FOR UPDATE SKIP LOCKED`, wait
  for `Dependency.Live`, nudge via `Watcher`).
- **Store constructors** for Postgres and KurrentDB still return
  `eventsourcing.EventStore`, so callers need
  `store.(eventsourcing.GlobalReader)` to build a runner.
- **Tutorial 4** still builds its cache on the event bus; it now points to
  the projection how-to. Rewriting it around the runner would be the next
  step for the user docs.

- **Guard against `Events` changing without a `Version` bump.** Adding an
  event type without bumping `Version` silently skips that type's history.
  Documented for now; a fix is to store a fingerprint of `Events` with the
  checkpoint and rebuild (with `Reset`) or refuse to start when it changes.
  Needs a schema column.
- **One reader per instance for segmented projections.** Each segment runs
  its own `ReadAll` and drops other segments' events, so N segments read
  the log N times. The old partitioned design had a per-instance
  dispatcher that read once and routed events to the owned segments.
  Documented as a cost for now.

## Tests to know about

- `projection/runner_test.go`: the runner on the memory store: catch-up,
  live, IsReplaying, rebuild, stall, segments, wiring panics.
- `projection/projectiontest/store.go`: the `Store` contract, run by
  `projection/memory/store_test.go` and (integration)
  `projection/postgres/store_test.go`.
