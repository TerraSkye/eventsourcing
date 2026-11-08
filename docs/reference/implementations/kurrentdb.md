# kurrentdb.EventStore

A KurrentDB (formerly EventStoreDB) implementation of `EventStore` and `GlobalReader`, built on the official [KurrentDB Go client](https://github.com/kurrent-io/KurrentDB-Client-Go). Stream storage, ordering and revision checks are delegated to the server.

## Package

```
github.com/terraskye/eventsourcing/kurrentdb
```

The package is its own Go module: `go get github.com/terraskye/eventsourcing/kurrentdb`. It requires Go 1.25.

## Constructor

```go
func NewEventStore(db *kurrentdb.Client, opts ...Option) *EventStore
```

| Parameter | Description |
|---|---|
| `db` | A connected `*kurrentdb.Client` from the KurrentDB client package. |

The returned `*EventStore` implements both `EventStore` and `GlobalReader`. `Close` closes the client.

```go
settings, err := kurrentdb.ParseConnectionString("esdb://localhost:2113?tls=false")
if err != nil {
    log.Fatal(err)
}
client, err := kurrentdb.NewClient(settings)
if err != nil {
    log.Fatal(err)
}
store := kdb.NewEventStore(client)
defer store.Close()
```

### Options

```go
type Option func(*EventStore)

func WithBackoff(newBackoff func() backoff.BackOff) Option
func WithLogger(logger *slog.Logger) Option
```

| Option | Default | Description |
|---|---|---|
| `WithBackoff` | exponential, initial 100ms, max interval 2s, max elapsed 30s | The `backoff.BackOff` (`github.com/cenkalti/backoff/v4`) `Save` retries transient errors with. `newBackoff` is called once per `Save`, so it must return a fresh backoff each time. |
| `WithLogger` | `slog.Default()` | Where `Save` reports each retried append, at warn level, with `stream`, `error` and `retry_in`. A nil logger keeps the default; pass `slog.New(slog.DiscardHandler)` for silence. |

## Behaviour

### Versions

KurrentDB numbers a stream's events from 0; this library numbers them from 1 and a `Revision` is a count of events. The conversion happens at this boundary only, so callers see the same numbering as on the other stores:

| This library | KurrentDB |
|---|---|
| `Envelope.Version` | `EventNumber + 1` |
| `Save(..., Revision(0))` | `NoStream{}` |
| `Save(..., Revision(N))` | `Revision(N - 1)` |
| `LoadStreamFrom(id, Revision(N))` | read from stream revision `N` |
| `AppendResult.NextExpectedVersion` | last `EventNumber` written + 1 |
| `Envelope.GlobalPosition` | the record's commit position in `$all` |

### Save

- All events must share one `StreamID` and carry an `EventID`; otherwise `Save` returns an error wrapping `ErrInvalidEventBatch` without contacting the server. An empty batch returns a successful `AppendResult` at once.
- Events are serialized as JSON with content type JSON, so event types must be registered (see [Event registry](../event-registry.md)). The envelope's `CorrelationID` and `CausationID` are stored in the event's metadata under KurrentDB's own `$correlationId` and `$causationId` keys, which its tooling and `$by_correlation_id` projection read; those keys in `Metadata` are dropped, since they would be read back as the chain.
- Every `StreamState` is supported: `Any`, `NoStream`, `StreamExists` and `Revision`. Another type returns an error wrapping `ErrInvalidRevision`.
- The append is retried with the backoff on transient errors: gRPC `Unavailable`, `DeadlineExceeded`, `ResourceExhausted` and `Aborted`, and a write that reached a follower instead of the leader (the client's `ErrorCodeNotLeader`). Nothing is written by a refused follower, so the next attempt goes to the leader. Once `ctx` is done the retries stop and the error wraps `ctx.Err()`.
- On success `AppendResult.GlobalPosition` is the commit position of the last event appended.

### LoadStream and LoadStreamFrom

Reads are lazy: the server only reports a missing stream once the first event is requested, so a violated precondition arrives through the iterator's `Err`, not from the call itself. Links are resolved.

| `version` | Behaviour |
|---|---|
| `Any` | Reads from the beginning. A missing stream is an empty read. |
| `Revision(N)` | Skips the stream's first `N` events. A missing stream is an empty read. |
| `StreamExists` (and `LoadStream`) | Reads from the beginning; a missing stream ends iteration with an error matching `ErrStreamNotFound`. |
| `NoStream` | Requires the stream not to exist; an existing one ends iteration with an error matching `ErrStreamExists`. |

### ReadAll and Head

- `ReadAll` reads `$all`. `GlobalPosition` is an event's commit position, so positions are not dense: lag measured in positions is a difference of log positions, not a count of events.
- A zero or negative `Limit` reads up to 500 events. The client cannot filter a plain read on the server, so `EventTypes` and `StreamPrefixes` are applied in Go, and one call looks at no more than `max(Limit, 4096)` records: a selective filter may return an empty page whose `Next` has moved forward, as the `GlobalReader` contract allows. `AtEnd` is set only when the read reached the end of `$all`.
- KurrentDB's own records in `$all` are skipped: events in system streams and event types starting with `$`, such as `$metadata` and link events. Links are not resolved, so an event reached through a link is not returned a second time. Skipped records still move `Next` forward.
- KurrentDB only makes a position readable once everything before it is, so reading `$all` is gap-free.
- `Head` reads `$all` backwards and returns the commit position of the newest application event, or 0 when there is none.

## Errors

Client errors are translated into this library's sentinels where one fits, with the client's own error left in the chain for `errors.As` and `kurrentdb.Error.Code`:

| KurrentDB error | Reported as |
|---|---|
| `ErrorCodeResourceNotFound`, `ErrorCodeStreamDeleted`, `ErrorCodeStreamTombstoned` | `ErrStreamNotFound` |
| `ErrorCodeResourceAlreadyExists` | `ErrStreamExists` |
| `ErrorCodeWrongExpectedVersion`, `ErrorCodeStreamRevisionConflict` on a `Save` with `Revision` | `*StreamRevisionConflictError`, with `ActualRevision` read from the stream after the failure (nil if that read fails) |
| the same, with `NoStream` | `ErrStreamExists` |
| the same, with `StreamExists` | `ErrStreamNotFound` |
| anything else (authentication, access control, node availability, size limits, parsing, internal errors) | wrapped as is, prefixed with the operation |

`io.EOF` is passed through unchanged: it is how the client signals the end of a stream, and `Iterator` relies on it. A deleted or tombstoned stream is reported as not found, like an absent one; a caller that must tell them apart matches the wrapped `*kurrentdb.Error` and compares its `Code()`.

## Limitations

- No projection store and no automation store: keep checkpoints in another package's, such as `postgres.NewProjectionStore`, next to the read model.
- `CorrelationLoader` is not implemented.
- Filters run in Go, so a filter that matches rarely scans up to 4096 records per `ReadAll` call.

## Related

- [Use KurrentDB as the event store](../../how-to/use-kurrentdb.md)
- [Retry after an unknown outcome](../../how-to/retry-an-unknown-outcome.md)
- [GlobalReader](../global-reader.md)
