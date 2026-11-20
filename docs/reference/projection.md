# projection

Keeps read models up to date from the global event log. A `Runner` reads events from a `GlobalReader`, routes each one to your handler, and commits the handler's writes together with its checkpoint through a `Store`, so a projection survives restarts, catches up on history, shares its segments between instances, and can be rebuilt from scratch.

[Tutorial 4](../tutorials/04-projections.md) and [Build a projection](../how-to/cached-projection.md) show how to write one. This page lists what the package exports.

## Package

```
github.com/terraskye/eventsourcing/projection
```

## Definition

```go
type Definition struct {
    Name         string
    Handler      eventsourcing.EventHandler
    EventTypes   []string
    Version      int
    Reset        func(ctx context.Context) error
    Segments     int
    SegmentKey   SegmentKey
    OnError      ErrorPolicy
    Logger       *slog.Logger
    Owner        string
    BatchSize    int
    PollInterval time.Duration
    ClaimTTL     time.Duration
}
```

A plain value; the zero value of every optional field picks a default.

| Field | Required | Default | Description |
|---|---|---|---|
| `Name` | yes | | Keys the checkpoint and the claim. Must be unique per read model and stable across deploys; renaming a projection makes it start from scratch. |
| `Handler` | yes | | Handles every event the runner reads, in log order: an `EventHandlerFunc` that switches on the event type, or an `*EventGroupProcessor`. |
| `EventTypes` | yes | | The registered names of the event types `Handler` handles; the runner reads only these. For an `EventGroupProcessor`, pass its `StreamFilter()`. Changing the list is detected; see [Rebuilding](#rebuilding). |
| `Version` | | 0 | The version of the projection's code, stored with the checkpoint. A segment whose stored version differs is rebuilt before processing. Only raise it; a runner whose `Version` is below the stored one stands by. 0 means no automatic rebuilds (`Rebuild` still works). Non-zero requires `Reset`. |
| `Reset` | with `Version` | | Wipes the read model, in the unit of work that moves the checkpoint back to zero, through the same context handlers use (such as `postgres.Tx(ctx)`). Required for `Version > 0` and for `Runner.Rebuild`. |
| `Segments` | | 1 | How many segments the projection is split into. 0 or 1 is a single segment that one instance runs at a time. Fixed per name; changing it takes a new name. |
| `SegmentKey` | with `Segments > 1` | | Picks which segment an event belongs to. `ByStreamID` fits read models whose rows are per stream. Must keep every read-model row within one segment. |
| `OnError` | | `StallOnError` | What the runner does when a handler fails on an event; see [ErrorPolicy](#errorpolicy). |
| `Logger` | | `slog.Default()` | Receives the runner's lifecycle; see [Logging](#logging). |
| `Owner` | | `hostname/pid` | Identifies this instance to the store. The runner appends a random part, as in `orders-7f9c/1a2b3c4d`, so every runner is an instance of its own; `Runner.Owner` returns the result. |
| `BatchSize` | | `DefaultBatchSize` = 500 | Maximum events read and committed in one unit of work. |
| `PollInterval` | | `DefaultPollInterval` = 1s | How often the runner reads again when nothing woke it, tries to claim segments it does not own, and refreshes an idle or stalled segment's claim. Must be shorter than `ClaimTTL`. |
| `ClaimTTL` | | `DefaultClaimTTL` = 15s | How long a claim stays valid without a `Commit` or `SaveStatus` to refresh it; a crashed instance's segments become available after this long. The claim is refreshed when a batch commits, not while its handlers run, so keep it above the time the slowest batch takes. |

### Segment keys

```go
type SegmentKey func(env *eventsourcing.Envelope) string

func ByStreamID(env *eventsourcing.Envelope) string
func SegmentOf(key string, segments int) int
```

`SegmentOf` maps a key to a segment in `[0, segments)` with FNV-32a, and returns 0 for `segments <= 1`. It is stable across processes and releases; `automation.ShardKey` uses it so a todo-list row and the projection that wrote it agree.

### ErrorPolicy

```go
type ErrorPolicy int
```

| Policy | `String()` | Behaviour |
|---|---|---|
| `StallOnError` | `stall_on_error` | Stops the segment at the failing event: the runner reports `Stalled`, backs off and retries until the handler succeeds or the projection is rebuilt. Nothing is skipped silently. Default. |
| `SkipWhenLive` | `skip_when_live` | Skips failing events while `Live` and not replaying; stalls on them while catching up, so a rebuild stays strict. |
| `SkipAlways` | `skip_always` | Skips failing events in every phase. |

Skipping dead-letters the event: the runner records it through `DeadLetterStore` when the store keeps them, and moves past it. A skipped event parks its stream: the segment skips that stream's later events too, recording each with `ErrStreamParked`, until `Runner.ReplayStream` applies them in order. This is not the skip of an `eventsourcing.SkippedEventError`, which a handler returns for an event it does not process and which is not a failure.

## Runner

```go
func NewRunner(p Definition, reader eventsourcing.GlobalReader, store Store) *Runner
```

Panics on wiring mistakes: an empty `Name`; a nil `Handler`, `reader` or `store`; no `EventTypes` or one that is not registered; `Version > 0` without `Reset`; `Segments > 1` without `SegmentKey`; a `PollInterval` no shorter than `ClaimTTL`.

### Methods

```go
func (r *Runner) Run(ctx context.Context) error
func (r *Runner) Rebuild(ctx context.Context) error
func (r *Runner) ReplayStream(ctx context.Context, streamID string) error
func (r *Runner) Nudge()
func (r *Runner) Watch(fn func())
func (r *Runner) Status() Status
func (r *Runner) IsLive(ctx context.Context) (bool, error)
func (r *Runner) WaitUntil(ctx context.Context, position uint64) error
func (r *Runner) DeadLettered() int64
func (r *Runner) Name() string
func (r *Runner) Owner() string
func (r *Runner) CanRebuild() bool
```

All methods other than `Run` are safe to call from any goroutine, before, during or after `Run`.

- `Run` processes events until `ctx` is done, then returns `ctx.Err()`. It returns early only when it cannot start: when reading the head of the log, `Store.Declare` or the first `Store.Heartbeat` keeps failing for `ClaimTTL`, with the last error, or at once when `Declare` returns `ErrSegmentsChanged`. Failures while processing never make it return. Call it once per `Runner`; on return it has released every claim it held and removed itself from the live instances.
- `Rebuild` records a request with `Store.RequestRebuild`, which the runner holding segment 0 serves between two batches, wherever it runs, then waits until the reset has run. A segmented projection is rebuilt as a whole. When `ctx` ends first, it returns `ctx.Err()` but the rebuild still runs. When the reset fails, the request stays pending, segment 0 stalls and retries, and `Rebuild` returns the error: as it was when the reset failed on this instance, as its text (recorded with `Store.FailRebuild`) when it failed on another. Returns `ErrNoReset` without a `Reset`, and an error wrapping `ErrUnknownProjection` before any runner declared the projection.
- `ReplayStream` applies the dead letters of `streamID` again, in order, on every segment that holds them, and un-parks the stream once they all apply. Each event is applied and its dead letter removed in one `Commit`; if the handler fails again, the stream stays parked. It waits until every segment has tried, and returns the segments' errors joined: as they were from this instance, as text from another. `ctx` only bounds the wait. Returns `ErrNoDeadLetterStore` when the store keeps no dead letters, and an error wrapping `ErrUnknownProjection` before any runner declared the projection.
- `Nudge` asks the runner to read now, on every segment it processes. It never blocks, nudges coalesce, and it only reaches this process's runner. Nudges reduce latency only; the poll always runs.
- `Watch` registers `fn` to be called after every committed batch that contained events and on every change of `Phase`. `fn` runs on the runner's goroutine and must not block; typically it is another component's `Nudge`.
- `Status` returns the status as seen by this instance. For a segmented projection it combines the segments: `Phase` is the furthest-behind phase of any segment (`Stalled`, then `Starting`/`Standby`, `CatchingUp`, `Live`), `Position` the lowest, `Head` the highest, `LastError` and `Owner` those of the lowest-numbered segment that has one.
- `IsLive` reports whether every segment is caught up with the head of the log, on whichever instance owns it: from memory for segments this instance owns, through `Store.LoadStatuses` and `Status.IsLive` otherwise.
- `WaitUntil` blocks until every segment has processed the log up to and including `position` (an `Envelope.GlobalPosition`, typically `AppendResult.GlobalPosition`), or until `ctx` is done, when it returns a `*NotReachedError`. Events the projection does not handle count as processed. While this instance owns every segment it watches memory; otherwise it waits on the store like a `Probe`, at `PollInterval`.
- `DeadLettered` returns the number of events dead-lettered since the runner was created. `Skipped` is its deprecated name.
- `CanRebuild` reports whether the projection has a `Reset`.

## Phases

```go
type Phase int
```

| Phase | `String()` | Meaning |
|---|---|---|
| `Starting` | `starting` | `Run` has not yet claimed and loaded this segment. |
| `Standby` | `standby` | Another instance holds the segment; this runner waits to take over if its claim expires. |
| `CatchingUp` | `catching_up` | The segment is behind the head of the log and reading full batches. |
| `Live` | `live` | The last read reached the head of the log. |
| `Stalled` | `stalled` | The last attempt failed; the runner is backing off. `Status.LastError` says why. |

## Status

```go
type Status struct {
    Phase            Phase
    Position         uint64
    Head             uint64
    Version          int
    LastError        string
    Owner            string
    Segment          int
    Age              time.Duration // only from Store.LoadStatuses
    ReplayUntil      uint64        // only from Store.LoadStatuses
    ClaimTTL         time.Duration // only from Store.LoadStatuses
    RebuildRequested bool          // only from Store.LoadStatuses
}

func (s Status) IsLive(claimTTL time.Duration) bool
```

`Head` minus `Position` is how far the segment lags behind. `Owner` is `""` when no instance holds the segment. `Age` is measured by the store's clock. `IsLive` reports a live segment as returned by `Store.LoadStatuses`: `Phase == Live`, held by an owner, and published no longer than its claim's ttl ago (`s.ClaimTTL` when the store reports it, else `claimTTL`).

The runner publishes its status to the store after every committed batch and on every change of phase.

## Lifecycle

1. **Start.** `Run` reads the head of the log, declares the segments with `Store.Declare` (events already in the log count as replayed for segments it creates), and sends the first `Heartbeat`. It retries all three with the stall backoff for up to `ClaimTTL`, then returns the last error. A membership goroutine keeps sending heartbeats; a replay goroutine watches for replay requests.
2. **Claim.** One loop per segment competes for the segment's claim through `Store.Claim`, while this instance holds fewer segments than its share. Without the claim it reports `Standby` and tries again every `PollInterval`. A segment whose stored `Version` is above the runner's is left to the newer deployment.
3. **Process.** With the claim, the segment is rebuilt first if its stored version differs from `Definition.Version`; then it reads batches of `BatchSize` after its checkpoint, filtered by `EventTypes`, drops the events of other segments, runs the handler on each, and commits the handler's writes, the new checkpoint and the status in one `Store.Commit`. At the head of the log it is `Live` and waits for a `Nudge` or `PollInterval`.
4. **Fail.** When a batch fails the segment reports `Stalled` with the error, keeps its claim (refreshing it every `PollInterval` so another instance does not take it over only to stall on the same event), backs off from 0.5s, doubling, to at most 30s, jittered, and retries from its last committed checkpoint. A segment that moved before failing starts its backoff over. `ErrClaimLost` drops the segment and goes back to step 2.
5. **Stop.** When `ctx` is done every segment releases its claim (best effort, under a two-second timeout), and the runner removes itself from the live instances with a ttl-0 heartbeat.

Exactly-once covers only what a handler writes through the store's unit of work, such as `postgres.Tx(ctx)`. Anything else a handler does happens at least once: a retried batch does it again.

### Rebuilding

- Bumping `Version` rebuilds every segment on the next start: `Store.Reset` runs `Reset` and moves every checkpoint back to zero in one unit of work, releasing every other segment's claim so instances still working one get `ErrClaimLost` and claim it again at the new checkpoint.
- `Runner.Rebuild`, `Store.RequestRebuild` and the [dashboard](./dashboard.md) rebuild on request, from any process with the store; requests made before a rebuild runs are all served by it. A request for a projection without a `Reset` stays pending, visible as `Status.RebuildRequested`, and is logged once.
- A change to `EventTypes` is detected through a fingerprint stored with the checkpoint. An unsegmented projection with a `Reset` rebuilds; otherwise the segment stalls with `ErrEventTypesChanged`, since carrying on would never apply the history of an added type. Bump `Version` with the change, or run the projection under a new name.
- During a rollout only instances on the highest `Version` get a share of the segments, so the first new instance runs the whole rebuild on its own.

### Several instances

Each runner records itself with `Store.Heartbeat` and computes its share from the instances listed: with S segments and N instances, S/N each, rounded down or up. It claims segments only up to its share and releases those above it between two batches, so a starting instance takes its share from the others, and a stopped one leaves at once (a crashed one after `ClaimTTL`).

Every call that acts on a claimed segment is fenced by the `Lease` the claim returned: its `Epoch` rises on every takeover, so a call made under an earlier claim, even from the same owner, is refused with `ErrClaimLost`.

## Replaying

```go
func IsReplaying(ctx context.Context) bool
```

Reports whether the event handled under `ctx` is being processed again: it was already in the log when the projection was first declared, or the projection has been rebuilt since it last saw it. Use it to skip work that only makes sense for new events, such as pushing a change to browsers. It is unrelated to catching up, and reports `false` when `ctx` was not passed to a handler by a `Runner`.

## Probe

```go
type ProbeDefinition struct {
    Name         string
    PollInterval time.Duration // default DefaultProbeInterval = 100ms
    ClaimTTL     time.Duration // default DefaultClaimTTL
}

func NewProbe(p ProbeDefinition, store Store) *Probe

func (p *Probe) Name() string
func (p *Probe) Instances(ctx context.Context) ([]Instance, error)
func (p *Probe) IsLive(ctx context.Context) (bool, error)
func (p *Probe) WaitUntil(ctx context.Context, position uint64) error
```

Follows a projection through the status its runners publish, without running it: for an API answering queries from a read model a worker keeps up to date. It needs only the store and the projection's name; `NewProbe` panics on an empty `Name` or a nil store. `ClaimTTL` is the `Definition.ClaimTTL` the projection runs with, used by `IsLive` when the store does not report the claim's own ttl.

`IsLive` and `Instances` read the store on every call; a projection no runner has started is not live. `WaitUntil` polls the store every `PollInterval` while a wait is in progress, sharing one read per interval between concurrent waits, so create one `Probe` per projection per process and share it. Only `ctx` ends an unsuccessful wait, with a `*NotReachedError`; a failing store does not. A `Probe` needs no closing.

`*Runner` and `*Probe` both satisfy `automation.Dependency`.

### NotReachedError

```go
type NotReachedError struct {
    Name     string
    Position uint64
    Statuses []Status // last seen, ordered by segment; empty when no runner had started
    StoreErr error    // the latest failed read of the store, if any
    Err      error    // the context's error
}

func (e *NotReachedError) Behind() *Status
func (e *NotReachedError) Unwrap() []error
```

Returned by `Runner.WaitUntil` and `Probe.WaitUntil` when the context ends the wait. `Behind` returns the segment that held the wait up, preferring a `Stalled` one, or nil; `Error` names it and, when stalled, its `LastError`. `Unwrap` returns `Err` and `StoreErr`, so `errors.Is` matches either.

## Store

```go
type Store interface {
    Declare(ctx context.Context, name string, segments int, replayUntil uint64) error
    Claim(ctx context.Context, l Lease, ttl time.Duration) (Lease, Checkpoint, bool, error)
    Commit(ctx context.Context, l Lease, cp Checkpoint, status Status, fn func(ctx context.Context) error) error
    Reset(ctx context.Context, l Lease, newVersion int, wipe func(ctx context.Context) error) error
    Release(ctx context.Context, l Lease) error
    SaveStatus(ctx context.Context, l Lease, status Status) error
    LoadStatuses(ctx context.Context, name string) ([]Status, error)
    RequestRebuild(ctx context.Context, name string) (uint64, error)
    Rebuilds(ctx context.Context, name string) (requested, done uint64, err error)
    FailRebuild(ctx context.Context, l Lease, requested uint64, errMsg string) error
    RebuildFailure(ctx context.Context, name string) (failed uint64, errMsg string, err error)
    Heartbeat(ctx context.Context, name, owner string, version int, ttl time.Duration) ([]Instance, error)
    LoadInstances(ctx context.Context, name string) ([]Instance, error)
}
```

Everything a projection persists: segment claims, checkpoints and status. One store serves every projection whose read models live in its database, and must sit in that database so read-model writes and checkpoint moves commit together. Implementations must be safe for concurrent use; `projectiontest.StoreAcceptanceTest` checks one against the contract.

- `Declare` creates the segments on first use with a zero checkpoint whose `ReplayUntil` is `replayUntil`; a different count later returns `ErrSegmentsChanged`.
- `Claim` takes the segment for `l.Owner` for `ttl`, refreshing the claim when `l` is the current one and otherwise taking it over when it is unowned or its heartbeat is older than the ttl it was taken with. It returns the claim's `Lease`, the checkpoint and `true`, or `false` when another claim holds the segment.
- `Commit` runs `fn` as one unit of work and, if it returns nil, stores `cp` and `status` and refreshes the heartbeat in it; the context passed to `fn` carries the unit of work (`postgres.Tx`, `sqlite.Tx`). If `fn` fails or `l` is no longer the current claim, everything is rolled back.
- `Reset` runs `wipe`, moves every segment's checkpoint back to position 0 with `newVersion`, marks every rebuild requested so far as done, releases every other segment's claim, and, on a `DeadLetterStore`, deletes the projection's dead letters and replay requests, all in one unit of work.
- `Release` gives the segment up at once and sets its phase to `Standby`.
- `SaveStatus` publishes `Phase`, `Head` and `LastError` between commits and refreshes the heartbeat.
- `Heartbeat` records `owner` as a live instance running `version` and returns the live instances; a `ttl` of 0 removes it. `LoadInstances` lists them without recording the caller.

Calls that take a `Lease` are fenced: when `l.Owner` no longer holds the segment or `l.Epoch` is not its current epoch, they change nothing, and `Commit`, `Reset`, `SaveStatus`, `FailRebuild` and `FinishReplay` return `ErrClaimLost`.

```go
type Lease struct {
    Name    string
    Segment int
    Owner   string
    Epoch   uint64 // fencing token; zero before the first claim
}

type Checkpoint struct {
    Position    uint64 // zero means nothing processed yet
    Version     int
    ReplayUntil uint64
    Fingerprint string // identifies the EventTypes; empty until the first commit
}

type Instance struct {
    Owner    string
    Version  int
    Segments int           // claims the instance holds
    Age      time.Duration // since its last heartbeat, by the store's clock
}
```

### DeadLetterStore

```go
type DeadLetterStore interface {
    SaveDeadLetter(ctx context.Context, l Lease, d DeadLetter) error
    LoadDeadLetters(ctx context.Context, name string, segment int) ([]DeadLetter, error)
    DeleteDeadLetter(ctx context.Context, l Lease, position uint64) error
    RequestReplay(ctx context.Context, name, streamID string) (uint64, error)
    PendingReplays(ctx context.Context, name string) ([]Replay, error)
    FinishReplay(ctx context.Context, l Lease, number uint64, errMsg string) error
    ReplayResult(ctx context.Context, name string, number uint64) (pending int, errs []string, err error)
}

type DeadLetter struct {
    Position  uint64
    StreamID  string
    EventType string
    Error     string // the handler's error, or ErrStreamParked
}

type Replay struct {
    Number   uint64
    StreamID string
    Segment  int
}
```

Optionally implemented by a `Store` to record events the runner skipped, so they can be inspected and replayed. Without it a skipped event is gone, and a parked stream is forgotten when the runner stops. `SaveDeadLetter` and `DeleteDeadLetter` are called inside the `Commit` that moves the checkpoint past the event or applies it again.

### Implementations

| Store | Notes |
|---|---|
| `memory.NewProjectionStore()` | Tests and in-memory read models. Cannot roll back `fn`'s writes, so handlers must be safe to apply twice; does not lock the segment while a batch runs, so a batch longer than `ClaimTTL` can lose its segment halfway. |
| `sqlite.NewProjectionStore(db)` | Each `Commit` and `Reset` runs in one transaction; handlers write through `sqlite.Tx(ctx)`. |
| `postgres.NewProjectionStore(pool)` | Each `Commit` and `Reset` runs in one transaction and locks the segment's row for the whole unit of work, so a slow batch delays a takeover rather than losing its work; handlers write through `postgres.Tx(ctx)`. |

All three implement `DeadLetterStore`.

## Errors

| Error | Returned by |
|---|---|
| `ErrClaimLost` | `Store.Commit`, `Reset`, `SaveStatus` and the other fenced calls when the owner no longer holds the segment. |
| `ErrSegmentsChanged` | `Store.Declare`, and so `Run`, when the projection exists with a different segment count. |
| `ErrUnknownProjection` | `Store.RequestRebuild` and `DeadLetterStore.RequestReplay`, and so `Rebuild` and `ReplayStream`, before any runner declared the projection. |
| `ErrNoReset` | `Rebuild` when the projection has no `Reset`. |
| `ErrEventTypesChanged` | Stalls a segment whose checkpoint was written for different `EventTypes` when the runner cannot rebuild. |
| `ErrStreamParked` | Recorded as the cause of a dead letter skipped because an earlier event of its stream was. |
| `ErrNoDeadLetterStore` | `ReplayStream` when the store does not implement `DeadLetterStore`. |
| `*NotReachedError` | `Runner.WaitUntil` and `Probe.WaitUntil` when the context ends the wait. |

## Logging

The runner logs its lifecycle to `Definition.Logger`: a segment claimed, released or lost (Info, Warn), a rebuild started and finished (Info), an event dead-lettered (Warn), a segment stalled with its error and `retry_in` (Error), and start, replay-poll and rebuild-failure problems (Warn). Every segment record carries `projection`, `segment`, `owner`, `position` and `head`. Nothing is logged per applied event or while idle; to log handler calls, wrap `Handler`, for example with `logging.WithEventLogging`.

## Telemetry

`telemetry.WithEventStoreTelemetry`, `telemetry.WithEventTelemetry`, `telemetry.WithProjectionStoreTelemetry` and `telemetry.ObserveProjection` instrument a runner; see the [telemetry reference](./telemetry.md) for the span and metric names.

## Related

- [Tutorial 4: Projections](../tutorials/04-projections.md)
- [Build a projection](../how-to/cached-projection.md)
- [Rebuild a projection without downtime](../how-to/rebuild-without-downtime.md)
- [GlobalReader](./global-reader.md), [automation reference](./automation.md), [dashboard reference](./dashboard.md)
