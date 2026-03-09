# How to handle concurrency conflicts with optimistic locking

Use `WithStreamState` and `WithRetryStrategy` to prevent lost updates when multiple processes write to the same aggregate stream simultaneously.

## The problem

By default, `NewCommandHandler` uses `Any{}` — it appends events without checking the current stream version. This is fine for most use cases, but if two goroutines handle commands for the same aggregate concurrently, the second write can silently overwrite the first.

## Solution: optimistic concurrency

Pass `WithStreamState` to require the stream to be at a specific revision before saving. If the revision has changed since you loaded events, the store returns `StreamRevisionConflictError`.

The framework tracks the latest version after loading and uses it automatically:

```go
import (
    "github.com/terraskye/eventsourcing"
    "github.com/cenkalti/backoff/v4"
)

handler := eventsourcing.NewCommandHandler(
    store,
    initialState,
    evolve,
    decide,
    eventsourcing.WithStreamState(eventsourcing.Any{}), // default — no check
)
```

To enforce optimistic locking, the framework automatically switches to `Revision(N)` after loading events. You do not need to set the revision manually — just ensure you're not using `Any{}` in the retry path.

## Add retry on conflict

When a conflict occurs, retry by reloading events and re-deciding:

```go
handler := eventsourcing.NewCommandHandler(
    store,
    initialState,
    evolve,
    decide,
    eventsourcing.WithRetryStrategy(
        backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 3),
    ),
)
```

The retry loop:
1. Reloads the stream from the last known revision.
2. Re-evolves state from the new events.
3. Re-runs `decide` with the updated state.
4. Attempts to save again.

This continues until success or the retry limit is reached.

## Ensuring a stream does not yet exist

To prevent duplicate creation (e.g., a resource with the same ID created twice), use `NoStream`:

```go
handler := eventsourcing.NewCommandHandler(
    store,
    initialState,
    evolve,
    decide,
    eventsourcing.WithStreamState(eventsourcing.NoStream{}),
)
```

If the stream already exists, `Save` returns `ErrStreamExists` immediately — no retry needed.

## Requiring a stream to exist

Use `StreamExists` when a command must only be applied to an existing aggregate:

```go
handler := eventsourcing.NewCommandHandler(
    store,
    initialState,
    evolve,
    decide,
    eventsourcing.WithStreamState(eventsourcing.StreamExists{}),
)
```

If the stream does not exist, `Save` returns `ErrStreamNotFound`.

## Stream states reference

| State | When to use |
|---|---|
| `Any{}` | No concurrency check needed (default) |
| `NoStream{}` | First write only — creation commands |
| `StreamExists{}` | Updates only — stream must already exist |
| `Revision(N)` | Set by the framework during retry — do not set manually |

## Handling `StreamRevisionConflictError`

If you do not use `WithRetryStrategy`, you can inspect the error yourself:

```go
result, err := handler(ctx, cmd)
if err != nil {
    var conflict *eventsourcing.StreamRevisionConflictError
    if errors.As(err, &conflict) {
        // conflict.Stream, conflict.ExpectedRevision, conflict.ActualRevision
        return fmt.Errorf("please retry: %w", err)
    }
    return err
}
```
