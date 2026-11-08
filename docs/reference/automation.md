# automation

Background processors that work off a todo list. An automation asks a projection (the todo list) what is pending, does the work for each item, and records the outcome by sending a command; the resulting event removes the item from the list.

[Tutorial 5](../tutorials/05-background-processing.md) builds one from scratch. This page lists what the package exports.

## Package

```
github.com/terraskye/eventsourcing/automation
```

## Definition

```go
type Definition[T any] struct {
    Name        string
    TodoList    TodoList[T]
    Projection  Dependency
    Store       Store
    Scheduler   Scheduler[T]
    Work        WorkFunc[T]
    OrderingKey func(item T) string
    WaitFor     []Dependency
    Concurrency  int
    MaxSegments  int
    BatchSize    int
    Lease        time.Duration
    PollInterval time.Duration
    Logger       *slog.Logger
}
```

`T` is the type of one claimed item. The zero value of every optional field picks a default.

| Field | Required | Default | Description |
|---|---|---|---|
| `Name` | yes | | Identifies the automation in logs, in its `Status` and in the `Store`. |
| `TodoList` | with `Store` | | The repository the work comes from. If it also implements `Segmented`, the runner uses that. Required unless `Scheduler` is set. |
| `Projection` | yes | | The projection that keeps `TodoList`. The automation only works while it is live. Pass a `*projection.Runner` when the projection runs in this program, or a `*projection.Probe` when it runs elsewhere. |
| `Store` | with `TodoList` | | Keeps segment leases, pauses and published status for the default scheduler. `memory.NewAutomationStore` for one process; `sqlite.NewAutomationStore` or `postgres.NewAutomationStore` when instances share a database. |
| `Scheduler` | | | Replaces the default scheduling with another source of keyed work. When set, `TodoList` and `Store` must be nil. |
| `Work` | yes | | Processes one claimed item; see [WorkFunc](#workfunc-and-deferred). |
| `OrderingKey` | | `nil` | Items with the same key run in claim order and never overtake each other, not even when one fails: a failed item parks its key for the rest of the batch. `nil` means every item is independent. |
| `WaitFor` | | | Projections that must be live, besides `Projection`, before the automation works. |
| `Concurrency` | | `DefaultConcurrency` = 1 | How many items of one segment run at once. Items with the same `OrderingKey` never run at once. |
| `MaxSegments` | | `DefaultMaxSegments` = 32 | Caps the segments this instance holds at once. |
| `BatchSize` | | `DefaultBatchSize` = 10 | Items claimed per segment at once. All are leased from the claim, so `BatchSize` times the time one item takes must stay well below `Lease`; an item whose lease ran out while it waited in the batch is counted as failed with "lease expired before work started", not run late. |
| `Lease` | | `DefaultLease` = 1m | How long claimed items are reserved for this instance, and the deadline `Work`'s context carries. |
| `PollInterval` | | `DefaultPollInterval` = 5s | How long an idle automation waits before checking for work again when nothing nudges it. |
| `Logger` | | `slog.Default()` | Receives transitions: waiting for a dependency, stalled, failed work, lost segments. Nothing per successful item or while idle. |

The package also exports `DefaultSegmentTerm` = 30s: how long the default scheduler keeps a held segment while other segments wait for an owner, before it gives it back.

## Runner

```go
func NewRunner[T any](d Definition[T]) *Runner[T]
```

Panics on wiring mistakes: an empty `Name`; a nil `Projection` or `Work`; neither `TodoList` nor `Scheduler`; a `Scheduler` together with a `TodoList` or a `Store`; a `TodoList` without a `Store`. It logs once, at Info, which capabilities it found on the list ("unsegmented", "segmented (N shards)" or "segmented (query)"), so a list that was meant to be segmented but is not shows up at startup. Each runner gets a random owner ID (a UUID) that it holds segments and publishes status under.

### Methods

```go
func (r *Runner[T]) Run(ctx context.Context) error
func (r *Runner[T]) RunOnce(ctx context.Context) (Summary, error)
func (r *Runner[T]) Nudge()
func (r *Runner[T]) Status() Status
func (r *Runner[T]) Name() string
```

- `Run` works until `ctx` is done, then returns `ctx.Err()`. Each round it publishes its status, checks the pause flag, checks that every dependency is live, and asks the scheduler for work. Each batch runs in its own goroutine, `Concurrency` items at a time, one at a time per `OrderingKey`; when a batch finishes, its segment can be claimed for again at once. If work was found, the next round starts right away; otherwise `Run` waits for the poll interval, a nudge, or a batch to finish. When a dependency implements `Watcher`, `Run` registers `Nudge` with it. Call `Run` once per `Runner`. On `ctx` done it waits for running batches, then closes the scheduler (under a 5s timeout) and returns.
- `RunOnce` works until nothing is eligible, gives every segment back, and returns what it did: the entry point for a cron job or a timer. A paused automation returns at once. A dependency that is not live makes it wait; a failing pause check, dependency check or scheduler call ends the run with that error (phase `Stalled`); a failing work call is counted and its item comes back once its lease expires. It is done when the scheduler has nothing more, no batch is running, and none finished since the scheduler was last asked.
- `Nudge` asks the automation to check for work now. It never blocks, and nudges coalesce.
- `Status` returns what the automation is doing on this instance.

## Phases

```go
type Phase int
```

| Phase | `String()` | Meaning |
|---|---|---|
| `Starting` | `starting` | `Run` has not checked its dependencies yet. |
| `Waiting` | `waiting` | A dependency is not live; `Status.WaitingFor` names it. |
| `Idle` | `idle` | The dependencies are live and the last check found nothing to do. |
| `Working` | `working` | Claimed items are being processed. |
| `Stalled` | `stalled` | A pause check, dependency check or scheduler call failed; the runner backs off and retries. `Status.LastError` has the error. |
| `Paused` | `paused` | Paused through the `Store`, for every instance; batches that were running finish. |

A stalled automation backs off from 0.5s, doubling, to at most 30s, jittered, and never gives up. The backoff resets after a successful scheduler call.

## Status

```go
type Status struct {
    Owner      string
    Phase      Phase
    WaitingFor string
    Processed  uint64        // work that succeeded since the automation started
    Failed     uint64        // work attempts that returned an error
    Deferred   uint64        // work attempts that returned Deferred; not failures
    LastError  string
    Age        time.Duration // only on a status from Store.LoadStatuses
}
```

Its shape follows `projection.Status`, so one dashboard reads both. The runner publishes it to the `Store` on every phase change and otherwise at most once per second, live for three times the larger of `PollInterval` and one second; `RunOnce` publishes once more when it returns. A failed publish is logged and the automation goes on.

## Summary

```go
type Summary struct {
    Processed, Failed, Deferred uint64
    NextEligible time.Time
}
```

What one `RunOnce` did. `NextEligible` is the earliest time a segment paused by `Deferred` during the run becomes eligible again, or zero when none was paused.

## TodoList

```go
type TodoList[T any] interface {
    Claim(ctx context.Context, req ClaimRequest) ([]T, error)
}

type ClaimRequest struct {
    Segments  []string      // empty for TodoList.Claim
    MaxPerKey int
    Lease     time.Duration
}
```

`Claim` reads pending items and leases them for `req.Lease`, so other instances skip them until the lease expires. The query's `ORDER BY` decides processing order. A claim must be atomic with respect to other instances: two instances claiming at once must never both get the same item (in SQL, `FOR UPDATE SKIP LOCKED`). An item whose lease expired without its work succeeding is pending again. `Claim` returns an empty slice, not an error, when nothing is pending. `automationtest.ClaimAcceptanceTest` checks an implementation against this contract.

### Segmented

```go
type Segmented[T any] interface {
    TodoList[T]
    Segments() Segmenter
    ClaimSegments(ctx context.Context, req ClaimRequest) ([]Claimed[T], error)
}

type Claimed[T any] struct {
    Segment string
    Items   []T
}
```

A list divided into segments by a column the projection writes. Each segment is leased to one instance at a time through the `Store`, so per-segment limits hold across instances, and a `Deferred` from `Work` pauses one segment rather than the whole list. `ClaimSegments` is `Claim` for the segments in `req.Segments`, which is never empty: up to `req.MaxPerKey` items per segment, grouped by segment. Segments without pending items are absent from the result; a segment the runner did not ask for is logged and skipped.

### Segmenter

```go
type Segmenter interface { /* unexported */ }

func Shards(n int) Segmenter
func ShardKey(id string, n int) string
func Query(fn func(ctx context.Context) ([]Segment, error)) Segmenter

type Segment struct {
    Key   string
    Group string
}
```

- `Shards(n)` segments a list into `n` fixed buckets, `"0"` to `"n-1"`; panics for `n <= 0`. Every bucket is a candidate on every poll, so no discovery query is needed; a `Deferred` pauses a whole bucket. The projection writes each item's bucket with `ShardKey(id, n)`, the same FNV-32a bucket `projection.SegmentOf` gives, so a todo-list row and the projection that wrote it agree.
- `Query(fn)` segments a list by what currently has pending work, typically `SELECT DISTINCT tenant_id FROM todo WHERE <pending>`. `fn` is called on every poll and should return only segments with work.
- `Segment.Group` is optional: when set, the store round-robins across groups before segments, so a tenant with forty segments and a tenant with one get the same number of turns.

## WorkFunc and Deferred

```go
type WorkFunc[T any] func(ctx context.Context, item T) error

type DeferredError struct {
    Until time.Time
}

func Deferred(until time.Time) error
```

`Work` does the work for one claimed item. Returning `nil` marks the item done. Returning an error leaves the item where it is; it is claimed again once its lease expires. Returning `Deferred(until)` pauses the item's whole segment until `until` without counting a failure, which is what a 429 with `Retry-After` means; a zero `until` means "back off for `Lease`". On an unsegmented list there is one segment, so a deferral pauses the whole automation on every instance.

The runner gives `Work` a context whose deadline is the item's lease end (`Item.Deadline`), with the correlation and causation IDs cleared: every item starts a new chain. To continue the chain of the event the item comes from, set them in `Work` with `eventsourcing.WithCorrelationID` and `eventsourcing.WithCausationID` before sending the command. An error that matches the runner's own cancelled context is not counted as a failure.

Work is delivered at least once. There is no attempt limit and no dead letter: an item whose work keeps failing is retried once per lease for as long as it stays on the todo list. Leases narrow the window in which two instances work the same item; they do not close it. Idempotency of the command and of any outgoing call is the user's job.

## Dependency and Watcher

```go
type Dependency interface {
    Name() string
    IsLive(ctx context.Context) (bool, error)
}

type Watcher interface {
    Watch(fn func())
}
```

`*projection.Runner` implements both; `*projection.Probe` implements `Dependency`. The automation checks `Projection` and every entry of `WaitFor` before every claim, not only at start. Items claimed before a dependency stopped being live are finished normally.

## Store

```go
type Store interface {
    Acquire(ctx context.Context, name, owner string, candidates []Segment, max int, ttl time.Duration) (AcquireResult, error)
    Renew(ctx context.Context, held []SegmentLease, ttl time.Duration) (lost []SegmentLease, err error)
    Release(ctx context.Context, rel []ReleaseRequest) (lost []SegmentLease, err error)
    Defer(ctx context.Context, name, key string, d time.Duration) error
    DeferredFor(ctx context.Context, name, key string) (time.Duration, error)
    SetPaused(ctx context.Context, name string, paused bool) error
    Paused(ctx context.Context, name string) (bool, error)
    SaveStatus(ctx context.Context, name string, status Status, ttl time.Duration) error
    LoadStatuses(ctx context.Context, name string) ([]Status, error)
    LoadSegments(ctx context.Context, name string) ([]SegmentState, error)
}
```

Keeps the shared state of an automation's instances: the segment leases of the default scheduler, whether the automation is paused, and what each instance last reported. Its methods and their signatures may change in minor releases before v1.0; external implementations should expect breakage. `automationtest.StoreAcceptanceTest` checks an implementation against the contract.

- `Acquire` records candidates it has not seen, then grants up to `max` eligible keys to `owner` for `ttl`. A key is eligible when nobody holds a live lease on it and its pause has passed. Candidates are ordered by the group that has waited longest, then by the key that has waited longest within it; keys with no group form one group. Every grant raises the key's epoch. `max` may be 0.
- `Renew` extends every lease in `held` by `ttl` and returns the ones the owner no longer holds.
- `Release` gives segments back, each fenced on owner and epoch. A released key is paused for `DeferFor` and, when `Served`, moves to the back of the fairness order.
- `Defer` pauses `key` for `d` from now, keeping any longer pause already set. It is not fenced. `key` is `""` for an unsegmented automation.
- `SetPaused` pauses or resumes the automation for every instance; pausing one the store has not seen is allowed.
- `SaveStatus` publishes a status, live for `ttl`; `LoadStatuses` returns the live ones ordered by `Owner`, with `Age` set.
- `LoadSegments` returns every segment the store has seen, ordered by key.

```go
type SegmentLease struct {
    Name, Key, Owner string
    Epoch            uint64
}

type ReleaseRequest struct {
    Lease    SegmentLease
    DeferFor time.Duration
    Served   bool
}

type AcquireResult struct {
    Granted  []SegmentLease // new leases, in grant order
    Active   int            // candidates whose pause has passed, held or not
    Eligible int            // candidates nobody holds whose pause has passed, after this grant
}

type SegmentState struct {
    Key, Group string
    Owner      string        // "" when nobody holds it
    PausedFor  time.Duration // left of a deferral, or 0
}
```

### Implementations

| Store | Scope |
|---|---|
| `memory.NewAutomationStore()` | One process. Another process does not see its leases. |
| `sqlite.NewAutomationStore(db)` | Every process that opens the same database file. Time is SQLite's clock in milliseconds. |
| `postgres.NewAutomationStore(pool)` | Every instance on the database. `Acquire` locks candidate rows in key order, so instances acquiring at once wait rather than deadlock. Time is the database's clock. |

The SQL stores use three tables, created by `postgres.Schema` and `sqlite.Schema`. The Postgres DDL:

```sql
CREATE TABLE IF NOT EXISTS automation_segments (
    name         TEXT        NOT NULL,
    segment_key  TEXT        NOT NULL,
    group_key    TEXT        NOT NULL DEFAULT '',
    owner        TEXT        NOT NULL DEFAULT '',
    epoch        BIGINT      NOT NULL DEFAULT 0,
    lease_until  TIMESTAMPTZ,
    paused_until TIMESTAMPTZ,
    first_seen   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_served  TIMESTAMPTZ,
    PRIMARY KEY (name, segment_key)
);

CREATE TABLE IF NOT EXISTS automation_controls (
    name   TEXT    PRIMARY KEY,
    paused BOOLEAN NOT NULL DEFAULT false
);

CREATE TABLE IF NOT EXISTS automation_instances (
    name        TEXT        NOT NULL,
    owner       TEXT        NOT NULL,
    phase       INT         NOT NULL,
    waiting_for TEXT        NOT NULL DEFAULT '',
    processed   BIGINT      NOT NULL DEFAULT 0,
    failed      BIGINT      NOT NULL DEFAULT 0,
    deferred    BIGINT      NOT NULL DEFAULT 0,
    last_error  TEXT        NOT NULL DEFAULT '',
    heartbeat   TIMESTAMPTZ NOT NULL DEFAULT now(),
    ttl_ms      BIGINT      NOT NULL,
    PRIMARY KEY (name, owner)
);
```

- `automation_segments` holds one row per segment the store has seen: who holds it, until when, and until when it is paused.
- `automation_controls` holds the automation-wide pause set with `SetPaused`.
- `automation_instances` holds what each instance last published with `SaveStatus`; a row is live for `ttl_ms` after its `heartbeat`.

The SQLite tables have the same columns with `INTEGER` for the timestamps (Unix milliseconds), epochs, counts and the `paused` flag.

## Scheduler

```go
type Scheduler[T any] interface {
    Next(ctx context.Context) ([]Batch[T], error)
    Done(ctx context.Context, item Item[T], out Outcome) error
    Drained(ctx context.Context, key string) error
    Close(ctx context.Context) error
}
```

The pluggable half of an automation: it decides which items this instance works now, for which segments, and what happens when an item finishes. The runner keeps the other half: ordering-key dispatch, `Work` deadlines, outcomes, `Run` and `RunOnce`. This interface may still change in minor releases before v1.0.

- `Next` returns the items this instance may work now, grouped by segment, or nothing. It must not block waiting for work, and must not return a batch for a segment whose previous batch has not drained.
- `Done` reports an item's outcome. For `DeferredOutcome` the scheduler pauses the item's segment until `Outcome.Until` for every instance.
- `Drained` reports that the batch for segment `key` (`""` for an unsegmented list) has finished.
- `Close` releases whatever the scheduler holds. The runner calls it once `Run` or `RunOnce` returns, with a 5s timeout.

`Next`, `Done` and `Drained` are called from several goroutines at once; an implementation must be safe for that. An implementation enforces the segment contract itself: at most one owner per segment at a time, and `Concurrency` items per segment. An automation with its own `Scheduler` has no `Store`, so it publishes no status and cannot be paused.

```go
type Item[T any] struct {
    Key      string    // the segment, "" for an unsegmented list
    Value    T
    Deadline time.Time // when the item's lease ends; Work's context carries it
}

type Batch[T any] struct {
    Key   string
    Items []Item[T]
}

type OutcomeKind int // Done, Failed, DeferredOutcome

type Outcome struct {
    Kind  OutcomeKind
    Until time.Time // for DeferredOutcome
    Err   error     // for Failed
}
```

### The default scheduler

Without `Definition.Scheduler`, the runner claims from the `TodoList`:

- **Unsegmented list.** One batch at a time per instance: `Next` checks `Store.DeferredFor(name, "")`, then calls `Claim` with `MaxPerKey` = `BatchSize` and `Lease`. Every instance claims independently (competing consumers). A deferral is written to the store at once with `Defer`, so other instances stop claiming.
- **Segmented list.** Each `Next` renews the held segments for a ttl of 3 × `Lease` + `PollInterval` and logs any that were lost; when other segments are waiting and this instance is at `MaxSegments`, it gives back the non-busy segment it has held longest past `DefaultSegmentTerm`; it then asks the `Segmenter` for candidates, acquires up to `MaxSegments` minus the held count, and claims for every free held segment in one `ClaimSegments` call. A held segment with nothing to claim is released at once as served, so it goes to the back of the fairness order, and paused for `PollInterval`, so the slot goes to a segment with work; with more buckets than `MaxSegments`, each poll probes the next bucket. A deferral is noted and applied when the batch drains: the segment is released, paused until the later of the deferral and the end of its batch's item leases.
- `Close` releases every held segment, with any pause a batch asked for.

## Pausing

`Store.SetPaused` pauses an automation for every instance at once: each stops claiming at its next poll, lets the items it is working on finish, gives its segments back, and reports `Paused`. `RunOnce` returns at once while paused. The [dashboard](./dashboard.md) offers the pause as a button.

## Logging

The runner logs to `Definition.Logger`: "automation configured" (Info, at `NewRunner`), "automation waiting for dependency" (Info, once per change), "automation stalled" (Error, with `error` and `retry_in`), "automation work failed" (Warn, with `segment` and `error`), "automation segment lost" (Warn), and failures to publish status, close the scheduler or report an outcome (Warn). Every record carries `automation`.

## Telemetry

`telemetry.WithWorkTelemetry`, `telemetry.WithAutomationStoreTelemetry` and `telemetry.ObserveAutomation` instrument an automation; see the [telemetry reference](./telemetry.md) for the span and metric names.

## Related

- [Tutorial 5: Background processing](../tutorials/05-background-processing.md)
- [projection reference](./projection.md) for the todo list's projection, `Runner.IsLive` and `Probe`
- [Add OpenTelemetry tracing](../how-to/add-opentelemetry.md)
