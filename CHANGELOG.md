# Changelog

All notable changes to this module are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the module
uses [semantic versioning](https://semver.org/). Before v1.0.0, a minor
release may break the API; every break is listed under **Breaking**.

## [Unreleased]

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
  checked by `projectiontest.StoreAcceptanceTest`.
- The `telemetry` event store wrapper traces `ReadAll` and `Head`, and
  implements `GlobalReader` only when the store it wraps does.
