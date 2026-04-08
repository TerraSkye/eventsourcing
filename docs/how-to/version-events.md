# How to version events

Handle event schema changes over time by introducing new versioned event types rather than mutating existing ones.

## The problem

Events in the store are immutable facts — they cannot be changed after being written. But the schema of an event often needs to evolve: a field is renamed, a new required field is added, or the structure changes significantly.

A common mistake is to try to "upcast" old events on read — silently transforming them into the new shape before they reach application code. This approach has a hidden cost: **it forces a single interpretation of the migration onto every consumer**. A projector that needs to distinguish whether an event was written before or after the schema change cannot do so.

The approach used here is simpler and more flexible: **introduce a new event type for each schema change and let each consumer handle the versions it cares about.**

## Introducing a new event version

When a schema change is needed, define a new event type alongside the existing one. Use a `V2` (or `V3`, etc.) suffix:

```go
// Original event — still in the store, never removed.
type TaskCreated struct {
    TaskID uuid.UUID
    Title  string
}

func (e *TaskCreated) EventType() string  { return "task.created" }
func (e *TaskCreated) AggregateID() string { return e.TaskID.String() }

// New version — written for all events from this point forward.
type TaskCreatedV2 struct {
    TaskID      uuid.UUID
    Title       string
    Description string  // newly required field
    CreatedBy   string  // newly required field
}

func (e *TaskCreatedV2) EventType() string  { return "task.created.v2" }
func (e *TaskCreatedV2) AggregateID() string { return e.TaskID.String() }
```

Your `decide` function now emits `TaskCreatedV2` for all new commands. Old `TaskCreated` events remain in the store exactly as they were written.

## Registering both versions

Register both types so the store can deserialize them:

```go
func init() {
    eventsourcing.RegisterEvent(&events.TaskCreated{})
    eventsourcing.RegisterEvent(&events.TaskCreatedV2{})
}
```

See [register events for serialization](./register-events.md) for background.

## Handling both versions in evolve

Your aggregate's `evolve` function handles whichever versions affect state. If the old and new versions both carry information relevant to the aggregate's state, handle both:

```go
func evolve(state TaskState, event eventsourcing.Event) TaskState {
    switch e := event.(type) {
    case *events.TaskCreated:
        state.TaskID = e.TaskID
        state.Title  = e.Title
        // Description and CreatedBy are absent — use zero values or defaults.
    case *events.TaskCreatedV2:
        state.TaskID      = e.TaskID
        state.Title       = e.Title
        state.Description = e.Description
        state.CreatedBy   = e.CreatedBy
    case *events.TaskCompleted:
        state.Done = true
    }
    return state
}
```

If the aggregate's business logic does not need the old event at all (for example, because you only create new tasks going forward), you can simply omit the old case and let it fall through.

## Handling both versions in projectors

Each projector independently decides how to handle each version. A simple summary projector might treat both versions the same way:

```go
func (p *TaskSummaryProjector) OnTaskCreated(ctx context.Context, e *events.TaskCreated) error {
    return p.store.Insert(TaskSummary{ID: e.TaskID, Title: e.Title})
}

func (p *TaskSummaryProjector) OnTaskCreatedV2(ctx context.Context, e *events.TaskCreatedV2) error {
    return p.store.Insert(TaskSummary{
        ID:          e.TaskID,
        Title:       e.Title,
        Description: e.Description,
        CreatedBy:   e.CreatedBy,
    })
}
```

A projector that builds a "created by user" index only subscribes to `TaskCreatedV2` and ignores the old version entirely — because the required data was simply not captured before the migration.

This is the core benefit of versioned events over upcasting: **each projector applies its own interpretation**. A projector that needs to know a field was missing can; a projector that doesn't care treats both versions equivalently.

## Renaming an event type (alternative to versioning)

If you want to change the Go type name but keep the stored event name the same, use `RegisterEventByName`:

```go
// Old events were stored as "task.created".
// The Go type was renamed from TaskCreated to TaskCreatedEvent.
eventsourcing.RegisterEventByName("task.created", func() eventsourcing.Event {
    return &events.TaskCreatedEvent{}
})
```

This is a rename, not a schema change — use it only when the struct fields have not changed.

## Guidelines

| Situation | Approach |
|---|---|
| Added a new optional field | New event version (`V2`), handle both in consumers |
| Added a new required field | New event version (`V2`), old version treated as "data not available" |
| Renamed a field | New event version (`V2`) — never rename fields on existing types |
| Renamed the Go type only (no field changes) | `RegisterEventByName` to map old stored name to new type |
| Removed a field | New event version (`V2`), drop the field from the new struct |

Never modify the fields of an existing event struct. Old events in the store were serialized with the old schema; changing the struct silently breaks deserialization for those events.
