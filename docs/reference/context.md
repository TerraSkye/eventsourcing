# Context helpers

When an event is dispatched through the event bus, the `Envelope` fields are injected into the context via `WithEnvelope`. Event handlers can read these values using the helper functions below.

## WithEnvelope

```go
func WithEnvelope(ctx context.Context, env *Envelope) context.Context
```

Injects all envelope fields into the context. Called automatically by the event bus before invoking handlers. You should not normally need to call this directly.

## Reader functions

```go
func StreamIDFromContext(ctx context.Context) string
func AggregateIDFromContext(ctx context.Context) string
func EventIDFromContext(ctx context.Context) uuid.UUID
func VersionFromContext(ctx context.Context) uint64
func GlobalVersionFromContext(ctx context.Context) uint64
func OccurredAtFromContext(ctx context.Context) time.Time
func MetadataFromContext(ctx context.Context) map[string]any
```

All functions return the zero value for their type if the key is not present in the context.

### Example

```go
func (p *Projector) OnTaskCreated(ctx context.Context, e *events.TaskCreated) error {
    eventID  := eventsourcing.EventIDFromContext(ctx)          // uuid.UUID
    streamID := eventsourcing.StreamIDFromContext(ctx)         // string
    version  := eventsourcing.VersionFromContext(ctx)          // uint64
    occurred := eventsourcing.OccurredAtFromContext(ctx)       // time.Time
    meta     := eventsourcing.MetadataFromContext(ctx)         // map[string]any

    log.Printf("[%s] event %s v%d at %s", streamID, eventID, version, occurred)
    _ = meta
    return nil
}
```

## Causation

```go
func WithCausation(ctx context.Context, causation string) context.Context
func CausationFromContext(ctx context.Context) string
```

Propagate a causation ID (e.g., the originating command or event ID) through the context for correlation in event chains.

```go
ctx = eventsourcing.WithCausation(ctx, commandID.String())
_, err := handler(ctx, cmd)

// In event handler:
cause := eventsourcing.CausationFromContext(ctx)
```
