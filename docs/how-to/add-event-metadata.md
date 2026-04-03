# How to add metadata to events

Use `WithMetadataExtractor` to attach contextual information (user ID, correlation ID, tenant) to every event envelope at the time it is saved.

## The problem

Event envelopes include a `Metadata map[string]any` field. You want to populate it automatically — for example with the authenticated user or a trace correlation ID — without cluttering your `decide` function.

## Solution: metadata extractors

A metadata extractor is a function `func(ctx context.Context) map[string]any`. Register one or more with `WithMetadataExtractor`:

```go
handler := eventsourcing.NewCommandHandler(
    store,
    initialState,
    evolve,
    decide,
    eventsourcing.WithMetadataExtractor(func(ctx context.Context) map[string]any {
        return map[string]any{
            "user_id": ctx.Value("user_id"),
        }
    }),
)
```

Every event saved by this handler will include `user_id` in its envelope metadata.

## Multiple extractors

You can register multiple extractors. Their outputs are merged:

```go
handler := eventsourcing.NewCommandHandler(
    store,
    initialState,
    evolve,
    decide,
    eventsourcing.WithMetadataExtractor(userMetadata),
    eventsourcing.WithMetadataExtractor(correlationMetadata),
)

func userMetadata(ctx context.Context) map[string]any {
    return map[string]any{"user_id": ctx.Value("user_id")}
}

func correlationMetadata(ctx context.Context) map[string]any {
    return map[string]any{"correlation_id": ctx.Value("correlation_id")}
}
```

If two extractors return the same key, the later one wins.

## Reading metadata from context in event handlers

When an event handler is called by the event bus, envelope fields are injected into the context via `WithEnvelope`. Use the context helpers to read them:

```go
func (p *Projector) OnTaskCreated(ctx context.Context, e *events.TaskCreated) error {
    meta    := eventsourcing.MetadataFromContext(ctx)
    eventID := eventsourcing.EventIDFromContext(ctx)
    version := eventsourcing.VersionFromContext(ctx)

    userID, _ := meta["user_id"].(string)
    log.Printf("task created by %s (event %s, version %d)", userID, eventID, version)
    return nil
}
```

## Available context helpers

| Function | Returns |
|---|---|
| `MetadataFromContext(ctx)` | `map[string]any` |
| `EventIDFromContext(ctx)` | `uuid.UUID` |
| `StreamIDFromContext(ctx)` | `string` |
| `AggregateIDFromContext(ctx)` | `string` |
| `VersionFromContext(ctx)` | `uint64` |
| `GlobalVersionFromContext(ctx)` | `uint64` |
| `OccurredAtFromContext(ctx)` | `time.Time` |

See [Context reference](../reference/context.md) for full documentation.
