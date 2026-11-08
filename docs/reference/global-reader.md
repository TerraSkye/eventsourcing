# GlobalReader

```go
type GlobalReader interface {
    ReadAll(ctx context.Context, after uint64, opts ReadOptions) (ReadResult, error)
    Head(ctx context.Context) (uint64, error)
}
```

Reads the global log, every event across all streams, in order, a page at a time. It is the one thing a store needs to support projections; catching up, waiting for new events, checkpointing and batching are done by the [projection](./projection.md) package on top of it.

`GlobalReader` is a separate interface from `EventStore` so that stores can adopt it independently, and so that code which only appends and loads streams does not depend on it. The memory, SQLite, Postgres and KurrentDB stores implement it next to `EventStore`; their constructors return the concrete store, so one value serves both.

## Package

```
github.com/terraskye/eventsourcing
```

## Methods

### ReadAll

```go
ReadAll(ctx context.Context, after uint64, opts ReadOptions) (ReadResult, error)
```

Returns the next page of the global log after position `after` (exclusive; `0` reads from the start). It does not block waiting for new events: at the head of the log it returns an empty page with `AtEnd` set, and callers poll to learn about new events.

### Head

```go
Head(ctx context.Context) (uint64, error)
```

Returns the `GlobalPosition` of the newest event a `ReadAll` started now could return, ignoring filters, or 0 if the log is empty. It is used to measure how far a reader lags behind and to record where a projection's history ends when it is declared. It should be cheap; implementations may return a slightly stale value, but never one beyond what `ReadAll` would return.

## ReadOptions

```go
type ReadOptions struct {
    Limit          int
    EventTypes     []string
    StreamPrefixes []string
}
```

The zero value reads every event, up to an implementation-chosen page size.

| Field | Description |
|---|---|
| `Limit` | The maximum number of events returned. Zero or negative lets the implementation pick a page size; every shipped store picks 500. `Limit` bounds the events returned, not the events scanned: a store that filters in Go may scan more rows than `Limit` to fill a page. |
| `EventTypes` | When not empty, restricts the result to events registered under one of these names. Matching is exact, even on backends whose native filter matches prefixes. `EventGroupProcessor.StreamFilter` returns a suitable value. |
| `StreamPrefixes` | When not empty, restricts the result to events whose `StreamID` starts with one of these prefixes. |

When both filters are set, an event must match both.

## ReadResult

```go
type ReadResult struct {
    Events []*Envelope
    Next   uint64
    AtEnd  bool
}
```

| Field | Description |
|---|---|
| `Events` | The matching events, in ascending `GlobalPosition` order. May be empty even when `Next` has advanced, if every event scanned was filtered out. |
| `Next` | The `GlobalPosition` of the last event the read scanned, whether it matched the filter or not. Pass it as `after` to the next call, and store it as a checkpoint once `Events` are processed. It lets a filtered reader move its checkpoint past events it is not interested in. When nothing was scanned, `Next` equals `after`. |
| `AtEnd` | The read reached the head of the log: at the moment of the read, no event with a `GlobalPosition` greater than `Next` was visible. It is a snapshot; new events may be appended right after. |

## Contract

Every implementation must uphold the following, and is checked against it by `eventsourcingtest.GlobalReaderAcceptanceTest`:

- **Order.** Events are returned in strictly ascending `GlobalPosition` order.
- **No gaps.** Once `ReadAll` has returned an event with `GlobalPosition` n, no event with a `GlobalPosition` of n or lower may become visible later. Stores where positions can become visible out of order, such as a SQL sequence assigned before commit, must hide events until every lower position is either committed or permanently abandoned.
- **Progress.** If `AtEnd` is false, `Next` is greater than `after`.
- **Exclusive start.** `ReadAll(ctx, n, ...)` never returns the event at position n itself. `GlobalPosition` 0 is never assigned to an event, so `after == 0` reads from the beginning.
- **Filters.** `EventTypes` and `StreamPrefixes` are always honoured, on the server when the backend can and in Go otherwise.

Because of the no-gaps rule, a reader that checkpoints `Next` never misses an event from a slower concurrent writer.

## Implementations

| Store | `GlobalPosition` | How the no-gaps rule is kept |
|---|---|---|
| [memory](./implementations/memory-store.md) | The event's index in the global slice, plus one. | A single slice under a mutex. |
| [sqlite](./implementations/sqlite.md) | The `events` table's `id`. | SQLite commits one writer at a time. Filters run in SQL. |
| [postgres](./implementations/postgres-store.md) | The `events` table's `id`. | Every append takes one shared advisory lock until commit, so ids become visible in order. Filters run in SQL. |
| [kurrentdb](./implementations/kurrentdb.md) | The record's commit position in `$all`; not dense. | KurrentDB only makes a position readable once everything before it is. Filters run in Go, scanning at most 4096 records per call; KurrentDB's own records are skipped. |

## Related

- [Tutorial 2: Your first query](../tutorials/02-first-query.md) reads the log directly
- [projection reference](./projection.md) for the runner built on it
- [Core interfaces](./interfaces.md)
