# Changelog

All notable changes to this module are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the module
uses [semantic versioning](https://semver.org/). Before v1.0.0, a minor
release may break the API; every break is listed under **Breaking**.

## [Unreleased]

### Breaking

- `automation.Remote` is removed. Pass a `projection.Probe` instead, which
  implements `automation.Dependency`:
  `projection.NewProbe(store, name, projection.WithClaimTTL(claimTTL))`.

### Added

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

- A stalled projection segment now publishes `Stalled` and its error to
  the store. Before, other instances kept seeing the phase of its last
  commit, usually `Live`, for as long as the stall lasted, as its claim
  stayed fresh while it retried.

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
