# Stream states

Stream states control the concurrency check applied when saving events or loading a stream.

```go
type StreamState interface {
    ToRawInt64() int64
}
```

---

## Any

```go
type Any struct{}
```

No caller-specified expectation of the stream's version. This is the **default** for `NewCommandHandler`.

Passed directly to `EventStore.Save`, `Any{}` means no concurrency check — events are always appended regardless of the current stream version.

`NewCommandHandler` behaves differently: it tracks the version it just loaded and saves against that instead, so a concurrent write is still detected as `*StreamRevisionConflictError` and, if `WithRetryStrategy` is configured, retried by reloading, re-deciding, and resaving until it converges. This is what "the framework manages Revision automatically" (below) refers to.

**Use when**: You don't need to pin a specific version yourself and want the handler to resolve conflicts by retrying.

---

## NoStream

```go
type NoStream struct{}
```

The stream must **not exist**. If a stream already exists, `Save` returns `ErrStreamExists` and `LoadStreamFrom` returns `ErrStreamExists`.

**Use when**: Creating a new aggregate and you want to prevent duplicate creation.

```go
handler := eventsourcing.NewCommandHandler(store, initial, evolve, decide,
    eventsourcing.WithStreamState(eventsourcing.NoStream{}),
)
```

---

## StreamExists

```go
type StreamExists struct{}
```

The stream **must exist**. If it doesn't, `Save` returns `ErrStreamNotFound` and `LoadStream` returns `ErrStreamNotFound`.

**Use when**: A command targets an existing aggregate and you want to fail fast if it was never created.

In `NewCommandHandler`, that "fail fast" only covers the missing-stream case: a load failure because the stream doesn't exist yet is always returned immediately, never retried, even with `WithRetryStrategy` configured — it's a precondition, not a version race. But once the stream is confirmed to exist, `StreamExists` isn't pinned to a specific version any more than `Any` is, so a subsequent save conflict is retried the same way `Any` converges (see [Any](#any) above).

---

## Revision

```go
type Revision uint64
```

The stream must be at exactly this version. If the actual version differs, `Save` returns `*StreamRevisionConflictError`.

**Use when**: You've read the stream at a specific version yourself (e.g. an API caller round-tripping a version from an earlier read) and want to detect — not silently absorb — a change since then. A conflict is always returned immediately, never retried: retrying would move past the exact version you pinned, defeating the reason you asserted it.

You should not normally set `Revision` manually just to get optimistic concurrency inside `NewCommandHandler` — use the default `Any{}` for that, which tracks the loaded version and retries on conflict automatically:

```go
// With Any{}, the framework tracks this internally after loading events
// and saves against it, instead of a caller-supplied Revision:
revision = eventsourcing.Revision(lastLoadedVersion)
```

---

## ToRawInt64 values

| State | `ToRawInt64()` | Meaning |
|---|---|---|
| `Any{}` | `-1` | No check |
| `NoStream{}` | `0` | Stream must not exist |
| `StreamExists{}` | `-2` | Stream must exist |
| `Revision(N)` | `N` | Exact version match |
