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
func GlobalPositionFromContext(ctx context.Context) uint64
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

## Correlation and causation

```go
func WithCorrelationID(ctx context.Context, correlationID string) context.Context
func CorrelationIDFromContext(ctx context.Context) string
func WithCausationID(ctx context.Context, causationID string) context.Context
func CausationIDFromContext(ctx context.Context) string
```

Every event records the chain it belongs to in two `Envelope` fields:

- `CorrelationID` is the ID of the chain's first message, copied unchanged to every event it caused, directly or not.
- `CausationID` is the `EventID` of the event whose handling produced this one, or `""` for the root of a chain, such as an event from a command an API request sent.

You rarely set them yourself. `WithEnvelope` puts a handler's work in the handled event's chain (causation = that event's ID, correlation = its `CorrelationID`), and `NewCommandHandler` copies both from the context onto the events it appends. With no correlation in the context, the events start a chain named after the first of them.

To tie a chain to an API request, start it with the request's ID:

```go
ctx = eventsourcing.WithCorrelationID(r.Context(), requestID)
_, err := bus.Dispatch(ctx, cmd)
```

Find every event of a chain with one query on the indexed column, for example in Postgres:

```sql
SELECT * FROM events WHERE correlation_id = $1 ORDER BY id;
```
