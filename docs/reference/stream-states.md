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

No concurrency check. Events are always appended regardless of the current stream version.

**Use when**: Order of writes doesn't matter, or you handle conflicts in application logic.

This is the **default** for `NewCommandHandler`.

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

---

## Revision

```go
type Revision uint64
```

The stream must be at exactly this version. If the actual version differs, `Save` returns `*StreamRevisionConflictError`.

**Use when**: Implementing optimistic concurrency. `NewCommandHandler` manages this automatically during retries — you should not normally set `Revision` manually.

```go
// The framework sets this internally after loading events:
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
