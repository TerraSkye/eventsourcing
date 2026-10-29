# Changelog

All notable changes to this module are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the module
uses [semantic versioning](https://semver.org/). Before v1.0.0, a minor
release may break the API; every break is listed under **Breaking**.

## [Unreleased]

### Breaking

- `Save` on every store in this module rejects a batch in which an
  envelope has no `EventID`, with an error wrapping
  `ErrInvalidEventBatch`. The Postgres and SQLite stores used to make
  one up, and the KurrentDB client made a new one on every retry
  attempt, so an append that committed but lost its response could be
  appended again under `Any`, or reported as a conflict under
  `Revision`. `NewCommandHandler` already sets one; code that calls
  `Save` directly must set `EventID` itself, for example to
  `uuid.New()`. `eventsourcingtest.AcceptanceTest` checks it.
- `projection.Store` has two new methods, `FailRebuild` and
  `RebuildFailure`, so `Runner.Rebuild` called on one instance returns
  the error of a reset that failed on another, as its text, instead of
  waiting until its context ends. Stores outside this module must
  implement both, and `StoreAcceptanceTest` checks them. The
  `projection_rebuilds` table gains a `failed` and an `error` column. For
  an existing database, run this once before deploying:

  ```sql
  -- Postgres
  ALTER TABLE projection_rebuilds ADD COLUMN failed BIGINT NOT NULL DEFAULT 0;
  ALTER TABLE projection_rebuilds ADD COLUMN error  TEXT;
  -- SQLite
  ALTER TABLE projection_rebuilds ADD COLUMN failed INTEGER NOT NULL DEFAULT 0;
  ALTER TABLE projection_rebuilds ADD COLUMN error  TEXT;
  ```

- `RegisterEventByType`, `RegisterEventByName`, `NewEventByName` and
  `EventNamesFor` are functions instead of package-level `var`s, so
  they can no longer be reassigned. Calls to them are unchanged.
- The Postgres, SQLite and KurrentDB stores reject an event at `Save`
  unless its type is registered under its `EventType()` name, with an
  error wrapping `ErrEventNotRegistered`. Such an event used to save
  and then fail on every read. Register every event type a store saves.
- The memory store now requires `RegisterEvent`, same as Postgres. It
  keeps each event encoded by `EncodeEvent` and decodes it on every
  read, so a type that is not registered fails at `Save`. Reads return
  the event as the registry makes it, a pointer: type assertions and
  switches must use `*OrderCreated`, not `OrderCreated`, even for an
  event saved as a value. Metadata comes back as JSON decodes it:
  numbers as `float64`, and nil as an empty map.
- `memory.NewEventStore` takes no arguments, and `EventStore.Events()`
  is removed. Read what was saved with `ReadAll`, or run a
  `projection.Runner` over the store.
- The event bus is removed: the `EventBus` interface and
  `SubscriberOption`, `memory.EventBus`, `postgres.EventBus`,
  `sqlite.EventBus` and `kurrentdb.EventBus` with their options
  (`WithFilterEvents`, `WithStartFrom`, `WithStartAt`,
  `WithStartAtZero`, `WithFilterStream`), and `telemetry.EventBus` with
  `WithEventBusTelemetry`. It did what `projection.Runner` does with
  fewer guarantees. Move each subscriber to a runner: its name becomes
  the `Definition`'s `Name`, its handler the `Handler`, and its event
  types the `EventTypes` (`StreamFilter()` for an
  `EventGroupProcessor`). A runner starts from the beginning of the
  log; the position a subscription had reached is not carried over.
  Apply event handler middleware by wrapping the handler, for example
  with `logging.WithEventLogging` or `telemetry.WithEventTelemetry`.
  `telemetry.EventBusTelemetry` is now `telemetry.EventTelemetry`.
- The schemas no longer create the `event_subscriptions` table, or on
  Postgres the trigger that sent a `NOTIFY` after every insert. Left in
  place they do no harm, but the trigger still runs on every insert.
  To remove them from an existing database, on Postgres:

  ```sql
  DROP TRIGGER IF EXISTS eventsourcing_events_notify ON events;
  DROP FUNCTION IF EXISTS eventsourcing_notify_events_inserted();
  DROP TABLE IF EXISTS event_subscriptions;
  ```

  and on SQLite:

  ```sql
  DROP TABLE IF EXISTS event_subscriptions;
  ```

- The metrics `WithEventTelemetry` records are renamed, since they
  measure any event handler: `eventsourcing.eventbus.handled` is
  `eventsourcing.event_handler.operations`,
  `eventsourcing.eventbus.duration` is
  `eventsourcing.event_handler.duration`, and
  `eventsourcing.eventbus.errors` is
  `eventsourcing.event_handler.errors`. Their Go variables are
  `EventHandlerOperations`, `EventHandlerDuration` and
  `EventHandlerErrors`. `AttrSubscriberName` is removed.

### Added

- `EncodeEvent` and `DecodeEvent`, and `EncodeMetadata` and
  `DecodeMetadata` for an envelope's metadata, are the JSON encoding
  every store in this module persists in. A store outside the module
  that calls them writes the same bytes.

### Fixed

- The memory store's `LoadStreamFrom` returned the stored envelopes
  themselves, so a caller changing one changed the store's copy, and
  `ReadAll` copied only the envelope, sharing the event and its
  metadata. Every read now decodes a new copy.

## [0.4.1] - 2026-10-24

### Changed

- The Postgres schema no longer adds columns to tables an earlier
  release created: like the SQLite one, it has the tables as this
  release uses them, and the changelog lists the statements to upgrade.
  When upgrading from before v0.4.0, run this once before applying the
  schema; `IF NOT EXISTS` skips what an earlier upgrade already added:

  ```sql
  ALTER TABLE events ADD COLUMN IF NOT EXISTS correlation_id TEXT NOT NULL DEFAULT '';   -- v0.3.0
  ALTER TABLE events ADD COLUMN IF NOT EXISTS causation_id   TEXT NOT NULL DEFAULT '';   -- v0.3.0
  ALTER TABLE projection_segments ADD COLUMN IF NOT EXISTS ttl_ms BIGINT NOT NULL DEFAULT 15000; -- v0.4.0
  ```

### Fixed

- The stores no longer keep every `ReplayStream` request forever.
  `FinishReplay` deletes a request once every segment tried it without
  an error, together with the earlier finished requests of its stream;
  a failed request is kept until then, or until `Reset`. A stream
  without dead letters records no request: `RequestReplay` returns 0.
  `StoreAcceptanceTest` checks the deleting, so a store outside this
  module that keeps its requests now fails it.
- `Runner.ReplayStream` returns an error a segment on its own instance
  failed with as it was, so `errors.Is` and `errors.As` work on it, as
  they did before 0.4.0. Only an error from a segment on another
  instance comes back as its text.
- A runner only reads replay requests from the store while one of its
  segments holds dead letters, and once after it claims a segment or
  serves a replay, instead of every `PollInterval` regardless. A failed
  read is logged as a warning, once until a read succeeds.

## [0.4.0] - 2026-10-24

### Breaking

- `projection.DeadLetterStore` has four new methods, `RequestReplay`,
  `PendingReplays`, `FinishReplay` and `ReplayResult`, which keep
  `Runner.ReplayStream` requests in the store. Stores outside this
  module must implement them, and `StoreAcceptanceTest` checks them. The
  Postgres and SQLite schemas add the `projection_replays` and
  `projection_replay_segments` tables. `projection.ErrNotActive` is
  removed: `ReplayStream` no longer needs a segment on the instance it
  is called on.
- The telemetry wrapper types drop the package name they repeated:
  `telemetry.TelemetryEventBus` is `telemetry.EventBus`,
  `telemetry.TelemetryStore` is `telemetry.EventStore` and
  `telemetry.TelemetryProjectionStore` is `telemetry.ProjectionStore`.
  The `With…Telemetry` constructors keep their names.
- `projection.NewProbe` takes a `projection.ProbeDefinition`, as
  `NewRunner` takes a `Definition`, instead of a name and options:
  `NewProbe(projection.ProbeDefinition{Name: "task-list", PollInterval: 50 * time.Millisecond}, store)`.
  `ProbeOption`, `WithPollInterval` and `WithClaimTTL` are removed.
  `NewProbe` panics on an empty `Name` or a nil store.
- A claim expires by the ttl its holder claimed it with, not by the
  ttl of the instance trying to take it over; instance rows expire the
  same way. The `projection_segments` and `projection_instances` tables
  gain a `ttl_ms` column. The Postgres schema adds it to an existing
  `projection_segments` itself. For an existing SQLite database, run
  this once before applying the new schema:

  ```sql
  ALTER TABLE projection_segments ADD COLUMN ttl_ms INTEGER NOT NULL DEFAULT 15000;
  ```

  Claims taken before the upgrade count with 15s, the default
  `ClaimTTL`, until they are claimed again.
- `projection.NewRunner` panics when `PollInterval` is not shorter than
  `ClaimTTL`. An idle segment refreshes its claim once per poll, so such
  a runner's claims expired between polls and moved on every one.
- A runner appends a random part to a configured `Definition.Owner`, as
  it already did to the default: `Owner: "orders-7f9c"` runs as
  `orders-7f9c/1a2b3c4d`. Every runner is then an instance of its own,
  so replicas given the same `Owner` split the segments instead of
  holding twice their share. `Status.Owner` and the logs show the full
  value, and `Runner.Owner` returns it.
- `WithRetryStrategy` takes a function that returns a `backoff.BackOff`
  instead of a `BackOff`. Wrap the existing value:
  `WithRetryStrategy(func() backoff.BackOff { return backoff.NewExponentialBackOff() })`.
- `projection.Store` has two new methods. `Heartbeat` records a runner
  as a live instance of its projection and lists the others as
  `projection.Instance`s; `LoadInstances` lists them without recording
  the caller. Stores outside this module must implement both, and
  `projectiontest.StoreAcceptanceTest` checks them.
- The Postgres and SQLite schemas add a `projection_instances` table. Apply
  the new schema before deploying: `Runner.Run` returns an error when its
  first heartbeat keeps failing for `ClaimTTL`.

### Added

- `Probe.Instances` lists a projection's live instances, including those
  on standby that hold no segment, through `Store.LoadInstances`. A
  dashboard or health check can watch them without becoming an
  instance with a share of the segments it never uses.
- The telemetry projection store records `eventsourcing.projection.heartbeats`,
  `eventsourcing.projection.instance.count` and
  `eventsourcing.projection.segment.held`, so a dashboard shows how the
  segments are spread and an alert can catch failing heartbeats.

### Fixed

- Closing a Postgres or SQLite `EventBus` while a handler was busy with
  an event lost that event's position, so the next bus delivered it
  again: the bus wrote positions with the subscriber's context, which
  `Close` cancels. On Postgres a whole batch of up to 100 events was
  rolled back. On SQLite the cancelled statement also made
  `database/sql` drop the one connection `Open` keeps, which threw a
  `:memory:` database away. The bus now runs its own statements under a
  context `Close` does not cancel, for up to five seconds, and handles
  no further event once closed (#181).
- `Runner.ReplayStream` only replayed the segments the instance it was
  called on held, and returned nil when the stream's segment was on
  another instance, leaving the stream parked. With segments spread
  over N instances that was most calls. The request now goes through
  the store, the instance holding each segment with dead letters of the
  stream serves it, and `ReplayStream` waits for all of them and returns
  their errors. It works from any process with the store, even one not
  running the projection.
- A one-off store or log error while `Runner.Run` started (reading the
  head, `Declare` or the first heartbeat) ended `Run`, while the same
  error later was tolerated. `Run` now tries the start again with the
  stall backoff for up to `ClaimTTL`, so a store that keeps failing,
  such as one missing a table, still fails `Run`. A changed segment
  count still fails at once.
- Instances configured with different `ClaimTTL` or `PollInterval`
  values, as during a rollout that changes them, judged each other's
  claims and heartbeats by their own ttl. One with a short ttl took over
  the segments of one that refreshes less often, and deleted its
  instance row, so the instances disagreed on how many there were and
  stopped shedding. `Status.ClaimTTL` now carries a claim's own ttl, and
  `Status.IsLive` uses it.
- A stalled segment kept its claim only until `ClaimTTL` ran out, while
  its retry backoff grows to 30s. Another instance then took the segment
  over and stalled on the same event. The segment now publishes its
  status every `PollInterval` during the backoff, which keeps the claim.
- A rebuild could keep a position from before its wipe: a segment that
  took its claim over while the reset ran kept its new lease, and a
  commit it had landed before the wipe reached the in-memory status.
  `WaitUntil` then returned while that segment's events were missing
  from the read model. The rebuild now retires every other segment's
  lease after the reset.
- A command handler built with `WithRetryStrategy` shared one `BackOff`
  across every command. On a `CommandBus` with several shards that was a
  data race, and commands lost or took each other's retries. Each command
  now gets a `BackOff` of its own from the function.
- The wait between retries ignored the command's context, so a cancelled
  command kept its `CommandBus` shard busy until the wait ended. The
  handler now binds each `BackOff` to the command's context.
- The segments of a projection spread across its instances (#182).
  Before, the first instance to start claimed every segment and the
  others stood by, so more instances added failover but no throughput.
  Now each runner claims up to its share of the segments, S/N rounded
  down or up, and releases the ones above it between batches. An instance
  that starts gets its share without any other stopping, and the segments
  of one that stops or crashes are divided over the rest. A single
  instance still holds every segment.

## [0.3.0] - 2026-10-22

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
- Package `file` is removed, as deprecated in v0.2.0. Before upgrading,
  copy a file store's events into SQLite with `sqlite.Import` on v0.2.0.
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

- `SkippedEventError` carries `Expected`, the type a handler built with
  `OnEvent` processes, and `Envelope`, the envelope the skipped event
  arrived in when `WithEnvelope` put one on the handler's ctx. Its message
  names both.
- `Envelope.CorrelationID` and `Envelope.CausationID` record the chain an
  event belongs to: the ID of the chain's first message, and the ID of the
  event whose handling produced it. `NewCommandHandler` copies both from
  the context onto the events it appends, and starts a chain named after
  the first event when the context has none. Postgres and SQLite store
  them in indexed columns, KurrentDB under its `$correlationId` and
  `$causationId` metadata keys (so `$by_correlation_id` works), and the
  memory store as fields.
- `NewCommandHandler` records the command in each event's `Metadata`, as
  `command_type` and a per-call `command_id`.
- `automation.Definition.Chain` returns the chain of the event that put an
  item on the todo list, so the events of its work join that chain.
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
- `eventsourcingtest.Benchmark` and `projectiontest.Benchmark` measure an
  event store and a projection store with the same code on every backend:
  append throughput on one stream and on 1,000 streams from 32 writers,
  `LoadStream` by stream length, `ReadAll` by page size, projection
  catch-up by segment count, and the time from an append to a live
  projection having processed it. The memory, SQLite, Postgres and
  KurrentDB benchmarks run behind the `bench` build tag; `BENCHMARKS.md`
  explains them and records results.

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
