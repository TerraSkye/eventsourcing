# How to handle concurrency conflicts with optimistic locking

Use `WithRetryStrategy` to resolve conflicts when several processes write to the same aggregate stream at once, and `WithStreamState` to state what the stream must look like.

## The problem

Two processes load the same stream at version 2. Both decide on that state and both append version 3. Whichever saves second decided on a state that no longer exists, so its event must not be stored as if nothing happened.

## Optimistic locking is on by default

`NewCommandHandler` saves against the version it just loaded. With the default `Any{}`, it saves with `Revision(N)`, where N is the last version it folded into state. A write that happened in between makes `Save` fail with `*StreamRevisionConflictError` instead of being stored. You never set the revision yourself:

```go
handler := eventsourcing.NewCommandHandler(
    store,
    initialState,
    evolve,
    decide,
    // eventsourcing.WithStreamState(eventsourcing.Any{}) is the default:
    // saved against the version just loaded.
)
```

Without a retry strategy, the conflict is returned to the caller.

## Add retry on conflict

When a conflict occurs, retry by reloading events and re-deciding:

```go
handler := eventsourcing.NewCommandHandler(
    store,
    initialState,
    evolve,
    decide,
    eventsourcing.WithRetryStrategy(func() backoff.BackOff {
        return backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 3)
    }),
)
```

The handler calls the function once per command, so every command gets its own retry budget, and a wait between retries ends when the command's context is cancelled.

If the handler runs behind a `CommandBus`, use a short retry such as `backoff.WithMaxRetries(&backoff.ZeroBackOff{}, 3)`. The wait between retries runs on the shard's worker and holds up every other command on that shard. See [Retry behind a CommandBus](../explanation/concurrency.md#retry-behind-a-commandbus).

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

If the stream already exists, the handler returns an error matching `ErrStreamExists` at once, without retrying.

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

If the stream does not exist, the handler returns an error matching `ErrStreamNotFound` at once, without retrying.

## Stream states reference

| State | When to use |
|---|---|
| `Any{}` | Most commands: saved against the version just loaded, conflicts retried (default) |
| `NoStream{}` | Creation commands: the stream must not exist yet |
| `StreamExists{}` | Updates only: the stream must already exist |
| `Revision(N)` | The caller read the stream at version N and asserts it is still there, such as an edit made against the version a user last saw. A conflict is returned, never retried |

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
