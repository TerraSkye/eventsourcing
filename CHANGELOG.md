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

### Added

- `eventsourcing.GlobalReader`, `ReadOptions` and `ReadResult`, with
  `eventsourcingtest.GlobalReaderAcceptanceTest` to check a store against
  the contract.
- `AppendResult.GlobalPosition`, the global position of the last event
  appended, set by every store.
- Package `projection`: a `Runner` that keeps a read model up to date from
  the global log, with checkpoints, claims across instances, segments,
  rebuilds, replay detection (`IsReplaying`), read-your-writes
  (`WaitUntil`) and dead letters. Stores in `projection/memory` and
  `projection/postgres`, checked by `projectiontest.StoreAcceptanceTest`.
- The `otel` event store wrapper traces `ReadAll` and `Head`, and
  implements `GlobalReader` only when the store it wraps does.
