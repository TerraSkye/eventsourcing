# How to add metadata to events

Use `WithMetadataExtractor` to attach contextual information (user ID, tenant) to every event envelope at the time it is saved. The chain an event belongs to is not metadata: it has its own `CorrelationID` and `CausationID` fields, which the library fills in; see [context helpers](../reference/context.md#correlation-and-causation).

## The problem

Event envelopes include a `Metadata map[string]any` field. You want to populate it automatically — for example with the authenticated user — without cluttering your `decide` function. `NewCommandHandler` already records the command in it, under `command_type` and a per-call `command_id`.

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
    eventsourcing.WithMetadataExtractor(tenantMetadata),
)

func userMetadata(ctx context.Context) map[string]any {
    return map[string]any{"user_id": ctx.Value("user_id")}
}

func tenantMetadata(ctx context.Context) map[string]any {
    return map[string]any{"tenant_id": ctx.Value("tenant_id")}
}
```

If two extractors return the same key, the later one wins.

## Reading metadata from context in event handlers

When the projection runner calls an event handler, envelope fields are injected into the context via `WithEnvelope`. Use the context helpers to read them:

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
| `GlobalPositionFromContext(ctx)` | `uint64` |
| `OccurredAtFromContext(ctx)` | `time.Time` |

See [Context reference](../reference/context.md) for full documentation.
