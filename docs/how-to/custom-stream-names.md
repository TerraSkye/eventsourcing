# How to customise stream names (multi-tenancy)

Override the stream naming strategy to prefix stream IDs with a tenant identifier or any other domain-specific scheme.

## Default behaviour

By default, a command handler uses the command's `AggregateID()` as the stream name. For a command with `AggregateID() = "order-123"`, events are stored in stream `"order-123"`.

## Per-handler override

Use `WithStreamNamer` to change the naming logic for a specific handler:

```go
handler := eventsourcing.NewCommandHandler(
    store,
    initialState,
    evolve,
    decide,
    eventsourcing.WithStreamNamer(func(ctx context.Context, cmd eventsourcing.Command) string {
        tenant := ctx.Value("tenant_id").(string)
        return fmt.Sprintf("%s-%s", tenant, cmd.AggregateID())
    }),
)
```

Streams are now stored as `"acme-order-123"`, `"globex-order-123"`, etc.

## Global override

Override `DefaultStreamNamer` at startup to apply the naming convention to all handlers:

```go
func init() {
    eventsourcing.DefaultStreamNamer = func(ctx context.Context, cmd eventsourcing.Command) string {
        if tenant, ok := ctx.Value("tenant_id").(string); ok && tenant != "" {
            return fmt.Sprintf("%s-%s", tenant, cmd.AggregateID())
        }
        return cmd.AggregateID()
    }
}
```

Be careful: changing `DefaultStreamNamer` after data has been written will break existing streams. Migrate data or set the namer before any events are stored.

## Type-prefixed streams

Another common pattern is to prefix with the aggregate type to avoid collisions across domains:

```go
eventsourcing.WithStreamNamer(func(ctx context.Context, cmd eventsourcing.Command) string {
    return fmt.Sprintf("order-%s", cmd.AggregateID())
})
```

## Notes

- The stream namer receives the full context so it can access auth claims, request metadata, or other values.
- The same stream name must be used consistently across all handlers that operate on the same aggregate. Mismatched names result in empty streams and incorrect state reconstruction.
