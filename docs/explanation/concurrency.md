# Concurrency and optimistic locking

## The problem

Event sourcing stores events in an append-only log. When two processes handle commands for the same aggregate simultaneously, both may read the same events, build identical state, and then try to save conflicting events. Without a concurrency mechanism, the second save silently wins and the first is lost.

## Optimistic locking

This library uses **optimistic concurrency control** (OCC): instead of locking the stream while a command is being processed, it checks the stream version at save time.

The sequence:

```
Process A                           Process B
---------                           ---------
LoadStreamFrom(id, Any{})           LoadStreamFrom(id, Any{})
  → events [v1, v2]                   → events [v1, v2]
  → state: {Exists:true}               → state: {Exists:true}

decide(state, cmdA)                 decide(state, cmdB)
  → [EventA]                          → [EventB]

Save([EventA], Revision(2))         Save([EventB], Revision(2))
  → success (stream now at v3)          → StreamRevisionConflictError!
                                           expected=2, actual=3
```

Process B's save fails because the stream moved to v3 while it was deciding.

## How the library handles this

With the default `Any{}` stream state, `NewCommandHandler` tracks the last event version after loading and uses that version as the expected revision when saving — the caller hasn't pinned a specific version, so the handler is free to check against whatever it just loaded. If another write happened concurrently, `Save` returns `*StreamRevisionConflictError`.

By default, the handler does **not** retry. To enable retries, pass `WithRetryStrategy`:

```go
import "github.com/cenkalti/backoff/v4"

handler := eventsourcing.NewCommandHandler(
    store, initialState, evolve, decide,
    eventsourcing.WithRetryStrategy(
        backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 5),
    ),
)
```

On conflict, the handler:
1. Reloads events from the last known revision (efficient — only fetches new events).
2. Re-applies `evolve` for the new events.
3. Re-runs `decide` with the updated state.
4. Retries `Save`.

This retry-and-converge behavior applies to `Any{}` and, once the stream is confirmed to exist, `StreamExists{}` — neither pins the stream to an exact version, so a save conflict can be resolved by reloading and retrying. `NoStream{}` and a specific `Revision(N)` pin the stream to an exact point (version 0, or N) the caller explicitly asserted — a conflict there is returned immediately as `*StreamRevisionConflictError`, regardless of `WithRetryStrategy`. Retrying those wouldn't converge: silently re-checking against a stream a `NoStream{}` caller wanted empty, or resaving a `Revision(N)` caller pinned to a version they specifically read, would defeat the reason those checks were requested in the first place (e.g. detecting a lost update in an "edit this document at the version I last saw" flow).

`StreamExists{}` also has a fail-fast side, distinct from a save conflict: if the stream doesn't exist *at all* when the handler loads it, that's a missing precondition, not a version race, and it's returned immediately — never retried — even with `WithRetryStrategy` configured. Only once loading succeeds (the stream is confirmed non-empty) does the "no version pinned, retry on conflict" behavior kick in for the subsequent save.

## CommandBus sharding

The `CommandBus` provides an alternative approach: commands for the same aggregate are always routed to the same worker shard (via FNV hash of `AggregateID()`). Within a shard, commands are processed sequentially. This eliminates conflicts without requiring retries, at the cost of reduced throughput per shard.

Combine both approaches for maximum resilience: CommandBus reduces conflicts, and retry handles the rare cases that still occur (e.g., from other processes).

## Stream states

The `StreamState` type controls which concurrency check is applied:

| State | Check | On conflict |
|---|---|---|
| `Any{}` | Checked against the version the handler just loaded (default) | retried per `WithRetryStrategy` |
| `NoStream{}` | Fail if stream exists | returned immediately, never retried |
| `StreamExists{}` | Fail if stream does not exist at load; once it does, checked against the loaded version like `Any{}` | load failure: returned immediately, never retried. Save conflict: retried per `WithRetryStrategy` |
| `Revision(N)` | Fail if stream is not at version N | returned immediately, never retried |

Use `NoStream{}` for creation commands to prevent duplicate aggregates:

```go
handler := eventsourcing.NewCommandHandler(
    store, initial, evolve, decide,
    eventsourcing.WithStreamState(eventsourcing.NoStream{}),
)
```

See [Stream states reference](../reference/stream-states.md) for details.

## When conflicts cannot happen

`Any{}` always checks against the version the handler loaded, but if only one process ever writes to an aggregate (guaranteed by the CommandBus or by design), or if the `decide` function is idempotent, conflicts simply won't occur — so there's no need to configure `WithRetryStrategy` at all.

For example, a processor that auto-archives tasks 30 days after completion might issue the same `ArchiveTask` command multiple times due to retries. Making `decide` return no events when the task is already archived eliminates the need for concurrency control entirely.
