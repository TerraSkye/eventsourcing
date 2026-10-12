# Handoff: projections (branch `feature/projections`)

Status as of 2026-10-12. This document is for whoever picks the branch up
next. Delete it when the branch is merged.

## Where things stand

| Area | State |
|---|---|
| `GlobalReader` (`ReadAll`/`Head`) on memory, file, postgres, kurrentdb | done; `EventStore.LoadFromAll` removed |
| `eventsourcingtest.GlobalReaderAcceptanceTest` | done; runs against all four stores |
| `projection.Runner`, unpartitioned (one active instance) | done |
| `projection.Runner` with `WithPartitions` (competing consumers) | done |
| `projection/postgres`: `Checkpoints`, `Leases`, `TryLock`, dead letters, status | done |
| `projection.Remote` | done |
| `automation` package | **design draft only**: signatures and docs; `Run` panics. Deliberately left out of scope for now. |

`go test -race -tags=integration ./...` passes; it needs Docker for
testcontainers (Postgres, KurrentDB). CI runs exactly that command.

## How a projection runs

Read the package docs first: `projection/doc.go`, then
`projection/partition.go` (the `WithPartitions` doc comment explains the
partitioned design in full) and `projection/postgres/checkpoints.go`.

**Unpartitioned** (`projection/single.go`): if the checkpoint store
implements `Locker`, every instance competes for a Postgres advisory lock
(class `"proj"`, key `hashtext(name)`, held on a dedicated pooled connection
that is pinged every second). The holder reads `ReadAll` pages after its
checkpoint and commits handler writes and the checkpoint in one transaction
(`postgres.Tx(ctx)` gives handlers that transaction). Others report
`Standby` and answer `Live`/`WaitUntil` from the published status.
A failed page is retried event by event to pin the failing event; then it
stalls (default) or skips to `projection_dead_letters` (`WithErrorPolicy`).
`WithVersion` + `WithReset` rebuild on start when the stored version
differs; `Runner.Rebuild` does it on demand.

**Partitioned** (`projection/partitioned.go`, `dispatcher.go`, `leases.go`):
ported from the standalone `partition` prototype (`partition.zip`). Each
instance heartbeats and claims `ceil(n/live)` partitions through a
`LeaseStore`, reads the log **once** per instance (`dispatcher`) and routes
each event to `PartitionOf(key(env), n)`. Each partition commits through
`CommitPartition`, which checks the lease's fence **before** running the
handlers and keeps the row locked, so a replaced instance can never commit
again and a new owner's `Acquire` waits for an in-flight batch. Rebuilds are
done under a new projection name (blue/green), not in place.

## Decisions worth revisiting

1. **Postgres appends are serialised.** The old global read could skip an
   event forever (ids 1 and 3 visible while 2's transaction was open). The
   fix in `eventstore/postgres` is one advisory lock shared by every `Save`
   (`appendLockKey1/2`), held until commit, so ids commit in order and
   `ReadAll` is a plain `id > $after`. Cost: appends to different streams no
   longer commit in parallel. The alternative is ordering the log by
   `(transaction id, id)`, which changes what `GlobalVersion` means.
   `TestReadAll_AppendsBecomeVisibleInIDOrder` pins the guarantee.
2. **Partitioned runners reject** `WithVersion`, skipping
   `WithErrorPolicy`, `WithOnCatchingUp` and `WithOnLive` (`NewRunner`
   panics), and ignore `WithRetryStrategy` (each partition backs off 0.5s
   to 30s on its own). Version rebuilds would need every instance to stop
   writing; skip policies and hooks just weren't ported yet.
3. **Status after a clean shutdown.** A stopped active runner leaves its last
   status (often `live`) in the store. Standby instances and `Remote`
   believe it until another instance takes over; `MaxAge` + `WithHeartbeat`
   is the guard. A `stopped` phase may be cleaner.
4. **First run counts history as replayed** (`ReplayUntil = head`). If a
   projection never commits before restarting (empty log), the boundary is
   recomputed on the next start.
5. **Memory checkpoints** (the default) cannot roll back, so delivery is
   at-least-once after a handler failure. The unit tests use a rollback-able
   fake (`txCheckpoints` in `projection/single_test.go`) to check
   exactly-once; the Postgres tests check it for real.

## Not done yet

- **Automations** (`automation/`): implement `Automation.Run` against the
  design in `automation/doc.go` (claim with `FOR UPDATE SKIP LOCKED`, wait
  for `Dependency.Live`, nudge via `Watcher`).
- **Redis lease store** from the prototype (`redisstore`): keeps heartbeats
  off the Postgres primary. Needs `github.com/redis/go-redis/v9`; the
  prototype's Lua scripts and its lease conformance suite (`leasetest`)
  are the starting point. A reusable `LeaseStore` conformance suite in
  `eventsourcingtest` would be worth having for it.
- **Store constructors** for Postgres and KurrentDB still return
  `eventsourcing.EventStore`, so callers need
  `store.(eventsourcing.GlobalReader)` to build a runner. Returning the
  concrete type, or an interface that embeds both, would remove that.
- **Notifier** is only implemented by the memory store. Postgres could use
  `LISTEN/NOTIFY` after commit; until then runners poll (`WithPollInterval`).
- **Example** of a table projection end to end (the prototype has
  `examples/orders` with a `main.go` and KEDA lag query) and user docs under
  `docs/` for projections.
- `roadmap.md` is untracked in the working copy and was updated for the
  `LoadFromAll` removal; it was not committed.

## Tests to know about

- `projection/single_test.go`: unit tests on the memory store: catch-up,
  live, IsReplaying, rebuild, version rebuild, stall/skip policies, hooks,
  lock failover with an in-process locker, `Remote`.
- `projection/postgres/partition_test.go` (integration): lease rules,
  fencing, `Acquire` waiting for an in-flight batch, exactly-once while
  instances leave and join mid-stream, restart, stall and recovery.
- `projection/postgres/single_test.go` (integration): advisory-lock
  failover, rebuild with `TRUNCATE`, dead letters, lock lost when its
  connection is terminated.

The tests were checked by breaking the code on purpose (duplicate delivery
in the dispatcher, no fence check, skipping regardless of policy); each
mutation made them fail.
