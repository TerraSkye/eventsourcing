# Changelog

All notable changes to this module are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the module
uses [semantic versioning](https://semver.org/). Before v1.0.0, a minor
release may break the API; every break is listed under **Breaking**.

## [Unreleased]

### Breaking

- Correlation and causation are fields of `Envelope`, set by the library,
  instead of a free-text label. `WithCausation` and `CausationFromContext`
  are removed; use `WithCorrelationID`, `CorrelationIDFromContext`,
  `WithCausationID` and `CausationIDFromContext`. `CausationID` is now
  the `EventID` of the event whose handling produced an event, never a
  command type or an automation's name.
- `WithEnvelope` sets the causation ID to the handled event's ID and the
  correlation ID to its `CorrelationID`, so the commands a handler sends
  join that event's chain.
- The telemetry wrappers no longer write `correlation_id` (the trace ID)
  or `causation_id` into `Metadata`; they record the chain as the span
  attributes `eventsourcing.correlation.id` and
  `eventsourcing.causation.id`. `logging.WithEventLogging` logs
  `event_id` and `correlation_id` instead of `causation`.
- The Postgres and SQLite `events` tables gain `correlation_id` and
  `causation_id` columns. The Postgres schema adds them to an existing
  table itself. For an existing SQLite database, run this once before
  applying the new schema, which otherwise fails with
  `no such column: correlation_id`:

  ```sql
  ALTER TABLE events ADD COLUMN correlation_id TEXT NOT NULL DEFAULT '';
  ALTER TABLE events ADD COLUMN causation_id   TEXT NOT NULL DEFAULT '';
  ```

  Rows written through the old telemetry wrapper carry the trace ID in
  their metadata. To keep it as their correlation in Postgres:

  ```sql
  UPDATE events
  SET correlation_id = convert_from(metadata, 'UTF8')::jsonb ->> 'correlation_id'
  WHERE correlation_id = ''
    AND metadata IS NOT NULL
    AND convert_from(metadata, 'UTF8')::jsonb ? 'correlation_id';
  ```

  The old `causation_id` metadata held a label, not an event ID, so it is
  not copied.
- `automation.Definition.Name` is no longer the causation of the work's
  commands.

### Added

- `Envelope.CorrelationID` and `Envelope.CausationID` record the chain an
  event belongs to: the ID of the chain's first message, and the ID of the
  event whose handling produced it. `NewCommandHandler` copies both from
  the context onto the events it appends, and starts a chain named after
  the first event when the context has none. Postgres and SQLite store
  them in indexed columns, KurrentDB under its `$correlationId` and
  `$causationId` metadata keys (so `$by_correlation_id` works), and the
  memory and file stores as fields.
- `NewCommandHandler` records the command in each event's `Metadata`, as
  `command_type` and a per-call `command_id`.
- `automation.Definition.Chain` returns the chain of the event that put an
  item on the todo list, so the events of its work join that chain.

- `telemetry.WithProjectionStoreTelemetry` traces each projection batch
  and rebuild and counts claims, and `telemetry.ObserveProjection` reports
  a runner's lag, position, phase and skipped events as gauges. Handlers
  wrapped with `WithEventTelemetry` show up as children of the batch.

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
