# Changelog

All notable changes to this module are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the module
uses [semantic versioning](https://semver.org/). Before v1.0.0, a minor
release may break the API; every break is listed under **Breaking**.

## [Unreleased]

### Breaking

- `projection.Store` has two more methods, `RequestRebuild` and
  `Rebuilds`, and `Reset` marks the requests it serves as done. The memory,
  Postgres and SQLite stores and the telemetry wrapper implement them; a
  store of your own needs them too. The Postgres and SQLite schemas add a
  `projection_rebuilds` table, which applying the schema creates on an
  existing database.
- `Runner.Rebuild` works from any instance and no longer returns
  `ErrNotActive`. It records a request in the store, which the runner
  holding segment 0 serves wherever it runs, and the request survives
  restarts and claims moving. Before any runner declared the projection
  it returns `ErrUnknownProjection`.
- `automation.Remote` is removed. Pass a `projection.Probe` instead, which
  implements `automation.Dependency`:
  `projection.NewProbe(store, name, projection.WithClaimTTL(claimTTL))`.
- `projection.Store.Reset` now resets every segment of the projection, not
  only the caller's: in one unit of work it runs wipe, moves every
  segment's checkpoint back, keeps the caller's claim and releases the
  others, and it waits for Commits in progress. A store that implements
  `DeadLetterStore` also deletes the projection's dead letters in that
  unit of work. The memory, Postgres and SQLite stores do this; a store
  outside this module must too, and `projectiontest.StoreAcceptanceTest`
  checks it.
- `projection.ErrRebuildSegmented` is removed, as segmented projections
  can now be rebuilt.

### Added

- `Store.RequestRebuild` asks for a rebuild from any program with the
  store, such as a web process, without running the projection.
  `Status.RebuildRequested` shows a pending one and `Status.ReplayUntil`
  how far a rebuilt segment is replaying. `Runner.CanRebuild` reports
  whether a projection has a Reset.
- Segmented projections rebuild in place. A `Definition.Version` bump or
  `Runner.Rebuild` wipes the read model once and moves every segment back
  to the start, and instances still working a segment are fenced off and
  resume from there. An `EventTypes` change still stalls a segmented
  projection unless `Version` is bumped with it.
- `projection.Probe` follows a projection from its store and name alone,
  for a process that reads a read model another process keeps up to date:
  `WaitUntil` and `IsLive`, with `WithPollInterval` and
  `WithClaimTTL`. Concurrent waits share one read of the store per
  interval, and a failing store delays waits rather than ending them. A
  wait its context ends returns a `*projection.NotReachedError`, whose
  `Behind` names the segment that held it up and whose `StoreErr` holds a
  store failure. `Runner.WaitUntil` returns it too, still matching the
  context's error with `errors.Is`. A `Probe` is an
  `automation.Dependency`.

- `telemetry.WithProjectionStoreTelemetry` traces each projection batch
  and rebuild and counts claims, and `telemetry.ObserveProjection` reports
  a runner's lag, position, phase and skipped events as gauges. Handlers
  wrapped with `WithEventTelemetry` show up as children of the batch.

### Fixed

- A rebuild runs a parked stream through the handler again. Before, the
  dead letters survived the reset and kept the stream parked for the
  whole replay, so even the events applied before the failure were left
  out of the rebuilt read model, and every event of the stream was
  recorded as a dead letter a second time.
- `Runner.ReplayStream` applies an event once when its position carries
  more than one dead letter, as the memory store can leave when a Commit
  fails after recording one.

- A runner whose `Definition.Version` is below the stored one, such as an
  instance of the previous deployment during a rollout, now stands by.
  Before, it rebuilt the projection back to its own version, and old and
  new instances kept resetting each other until the rollout finished.

- A stalled projection segment now publishes `Stalled` and its error to
  the store. Before, other instances kept seeing the phase of its last
  commit, usually `Live`, for as long as the stall lasted, as its claim
  stayed fresh while it retried.

- The stall backoff grows from 0.5s to 30s as documented. Before, every
  retry claimed the segment again, which reset it, so a failing handler
  was retried about twice a second forever.
- `Runner.Nudge` wakes every segment. Before, it woke one, and the others
  waited out their `PollInterval`, so `WaitUntil` after a `Nudge` on a
  segmented projection took up to a full interval.
- `Watch` callbacks run when a segment goes `Live` without applying
  events, such as on a restart with the checkpoint at the head. Before,
  an `automation` waiting on such a projection idled for its `Interval`.
- After `Run` returns, `IsLive` and `WaitUntil` ask the store. Before, the
  stopped runner kept reporting itself as the live owner.
- A `Claim` that fails with a store error no longer makes the runner
  report itself as the segment's owner, so `IsLive` and `WaitUntil` ask
  the store, where the real owner's progress is.
- When a batch is retried event by event, the store's checkpoint now
  moves to the end of the read, past events the segment does not handle.
  Before, it stayed at the last handled event, and waits on the store for
  the scanned positions hung until a new event arrived.
- A rebuild whose reclaim fails after `Store.Reset` committed, and a
  `Rebuild` that returns while its rebuild is still under way on this
  instance, no longer let `WaitUntil` see positions from before the
  wipe. A `Probe` wait no longer answers from statuses read before a
  rebuild by an earlier wait.
- `Store.Reset` keeps the larger of the current `ReplayUntil` and the
  position reached. Before, a rebuild during the replay lowered it, and
  `IsReplaying` reported events already in the log as new.
- `ReplayStream` calls queued behind a replay that lost the claim return
  `ErrNotActive`. Before, they waited until their ctx ended.

## [0.2.0] - 2026-10-18

### Breaking

- `EventStore.LoadFromAll` is removed. Read the global log through the new
  `eventsourcing.GlobalReader` (`ReadAll`, `Head`) instead, which the
  memory, file, Postgres and KurrentDB stores implement. Unlike
  `LoadFromAll`, it guarantees ascending order with no gaps (on the file
  store only with a single writer), pages with `Limit`, filters by event
  type and stream prefix, and reports where to resume with
  `ReadResult.Next`.
- `postgres.NewEventStore` and `kurrentdb.NewEventStore` return
  `*EventStore` instead of `eventsourcing.EventStore`, so callers get
  `GlobalReader` without a type assertion.
- Postgres: every `Save` now takes one advisory lock shared by all appends,
  held from the inserts to the commit. Ids then become visible in the
  order they were assigned, which `GlobalReader` needs, but appends to
  different streams no longer commit in parallel. Write throughput of a
  busy Postgres store goes down; encoding events happens before the lock.
- `Envelope.GlobalVersion` is renamed `GlobalPosition`, and
  `GlobalVersionFromContext` is renamed `GlobalPositionFromContext`: it is
  a position in the global log, like `ReadAll`'s `after` and a projection
  checkpoint, while a version counts events within one stream. The
  logging middleware's `global_version` attribute is now
  `global_position`. The file store and file bus keep `global_version` as
  the JSON name, so existing files still load.
- Package `otel` is renamed `telemetry` and moves to
  `github.com/terraskye/eventsourcing/telemetry`, so it no longer shares
  its name with `go.opentelemetry.io/otel`, which every user imports
  alongside it. Only the import path and the package name change.

- Backends are laid out one package per backend instead of one per
  concern. Each backend package offers `NewEventStore`, `NewEventBus` and,
  where it has one, `NewProjectionStore`:

  | Before | After |
  |---|---|
  | `eventstore/memory.NewMemoryStore` | `memory.NewEventStore` |
  | `eventbus/memory.NewEventBus` | `memory.NewEventBus` |
  | `projection/memory.NewStore` | `memory.NewProjectionStore` |
  | `eventstore/file.NewFileStore` | `file.NewEventStore` |
  | `eventbus/file.NewFileEventBus` | `file.NewEventBus` |
  | `eventstore/postgres.NewEventStore` | `postgres.NewEventStore` |
  | `eventbus/postgres.NewEventBus` | `postgres.NewEventBus` |
  | `projection/postgres.NewStore` | `postgres.NewProjectionStore` |
  | `eventstore/kurrentdb.NewEventStore` | `kurrentdb.NewEventStore` |
  | `eventbus/kurrentdb.NewEventBus` | `kurrentdb.NewEventBus` |

  `postgres.Schema` (also shipped as `postgres/schema.sql`) creates every
  table the package uses, replacing `eventstore/postgres/schema.sql` and
  `eventbus/postgres/schema.sql`.

- `postgres`, `kurrentdb` and `telemetry` are separate Go modules, each
  with its own `go.mod`; their import paths do not change, but each is
  fetched with its own `go get`. The root module keeps the core
  interfaces, `memory`, `file`, `projection` and the acceptance suites,
  and no longer pulls in pgx, the KurrentDB client, gRPC, testcontainers
  or OpenTelemetry. It requires Go 1.24; `postgres` and `kurrentdb`
  require Go 1.25.

### Deprecated

- Package `file`. Use `sqlite` instead; `sqlite.Import` copies a file
  store's events into SQLite. `file` will be removed in a later release.

### Added

- `eventsourcing.GlobalReader`, `ReadOptions` and `ReadResult`, with
  `eventsourcingtest.GlobalReaderAcceptanceTest` to check a store against
  the contract.
- `AppendResult.GlobalPosition`, the global position of the last event
  appended, set by every store.
- Package `projection`: a `Runner` that keeps a read model, described by a
  `projection.Definition`, up to date from the global log, with
  checkpoints, claims across instances, segments, rebuilds, replay
  detection (`IsReplaying`), read-your-writes (`WaitUntil`) and dead
  letters. Stores in `projection/memory` and `projection/postgres`,
  checked by `projectiontest.StoreAcceptanceTest`. A checkpoint records
  which event types it was written for, so changing a projection's
  `EventTypes` rebuilds it, or stalls it with `ErrEventTypesChanged`
  when it cannot rebuild. A skipped event parks its stream: later events
  of the stream are recorded as dead letters instead of applied, until
  `Runner.ReplayStream` applies them in order. The runner logs its
  lifecycle (claims, releases, rebuilds, skipped events, stalls) to
  `Definition.Logger`, `slog.Default()` unless set.
- Package `sqlite`, its own module: an event store, an event bus and a
  projection store in one SQLite database on the pure-Go
  `modernc.org/sqlite` driver, for development, tests that need real
  transactions, and single-process services. It passes the same
  acceptance suites as the Postgres store, in memory and on disk.
- Package `automation` runs Event Modeling automations: `automation.New`
  takes a `Definition[T]` with a todo list to wait for, a `ClaimFunc` that
  leases pending items and a `WorkFunc` for one item, claims only while
  the todo list and `WaitFor` dependencies are live, and reports a
  `Phase` and counts like a projection. `automationtest.ClaimAcceptanceTest`
  checks a todo list's claim. Tutorial 5 is rewritten around it.
- The `telemetry` event store wrapper traces `ReadAll` and `Head`, and
  implements `GlobalReader` only when the store it wraps does.
