# Implementing Command Handlers

This guide explains how to implement command handlers using the **Evolve + Decide** pattern in terraskye/eventsourcing.

## Overview

Command handlers process commands (user intentions) and produce events (facts about what happened). The framework uses a functional pattern with two pure functions:

1. **Evolve** - Reconstructs aggregate state from events
2. **Decide** - Applies business rules and produces new events

```
┌─────────┐     ┌──────────────────────────────────────┐     ┌────────┐
│ Command │────▶│  Load Events → Evolve → Decide       │────▶│ Events │
└─────────┘     │                                      │     └────────┘
                │  1. Load event history               │          │
                │  2. Replay events to get state       │          ▼
                │  3. Apply business rules             │    ┌───────────┐
                │  4. Return new events                │    │EventStore │
                └──────────────────────────────────────┘    └───────────┘
```

## Step 1: Define Your Events

Events are immutable facts. Name them in past tense.

```go
package events

import "github.com/google/uuid"

type SeatReserved struct {
    ScreeningID uuid.UUID `json:"screening_id"`
    SeatNumber  string    `json:"seat_number"`
    UserID      uuid.UUID `json:"user_id"`
}

func (e SeatReserved) AggregateID() string { return e.ScreeningID.String() }
func (e SeatReserved) EventType() string   { return "SeatReserved" }

type SeatReleased struct {
    ScreeningID uuid.UUID `json:"screening_id"`
    SeatNumber  string    `json:"seat_number"`
}

func (e SeatReleased) AggregateID() string { return e.ScreeningID.String() }
func (e SeatReleased) EventType() string   { return "SeatReleased" }
```

Register events at application startup:

```go
func init() {
    eventsourcing.RegisterEvent(events.SeatReserved{})
    eventsourcing.RegisterEvent(events.SeatReleased{})
}
```

## Step 2: Define Your Command

Commands express intent. Name them as imperative actions.

```go
package reserveseat

import "github.com/google/uuid"

type ReserveSeat struct {
    ScreeningID uuid.UUID
    SeatNumber  string
    UserID      uuid.UUID
}

// AggregateID identifies which aggregate this command targets
func (c ReserveSeat) AggregateID() string {
    return c.ScreeningID.String()
}
```

## Step 3: Define the Aggregate State

The state holds only what's needed for business decisions. It's reconstructed from events on every command.

```go
package reserveseat

type screeningState struct {
    ReservedSeats map[string]bool // seat number -> reserved
}

var initialState = screeningState{
    ReservedSeats: make(map[string]bool),
}
```

## Step 4: Implement the Evolver

The evolver is a pure function that applies a single event to the current state, returning the new state.

```go
func evolve(state screeningState, env *eventsourcing.Envelope) screeningState {
    switch e := env.Event.(type) {
    case *events.SeatReserved:
        // Copy map to maintain immutability
        newSeats := make(map[string]bool)
        for k, v := range state.ReservedSeats {
            newSeats[k] = v
        }
        newSeats[e.SeatNumber] = true
        return screeningState{ReservedSeats: newSeats}

    case *events.SeatReleased:
        newSeats := make(map[string]bool)
        for k, v := range state.ReservedSeats {
            newSeats[k] = v
        }
        delete(newSeats, e.SeatNumber)
        return screeningState{ReservedSeats: newSeats}
    }

    return state // Unknown event, return unchanged
}
```

**Key rules for evolvers:**
- Must be a pure function (no side effects)
- Never fail - events are facts that already happened
- Return a new state, don't mutate the input
- Handle all relevant event types

## Step 5: Implement the Decider

The decider contains your business logic. It receives the current state and command, then returns events or an error.

```go
func decide(state screeningState, cmd ReserveSeat) ([]eventsourcing.Event, error) {
    // Business rule: seat must not already be reserved
    if state.ReservedSeats[cmd.SeatNumber] {
        return nil, fmt.Errorf("seat %s is already reserved", cmd.SeatNumber)
    }

    // All rules passed - emit the event
    return []eventsourcing.Event{
        &events.SeatReserved{
            ScreeningID: cmd.ScreeningID,
            SeatNumber:  cmd.SeatNumber,
            UserID:      cmd.UserID,
        },
    }, nil
}
```

**Key rules for deciders:**
- Return an error for business rule violations
- Return an empty slice if no state change is needed (idempotent commands)
- Can return multiple events for a single command
- Must be deterministic given the same state and command

## Step 6: Create the Handler

Wire everything together with `NewCommandHandler`:

```go
package reserveseat

import "github.com/terraskye/eventsourcing"

func NewHandler(store eventsourcing.EventStore) eventsourcing.CommandHandler[ReserveSeat] {
    return eventsourcing.NewCommandHandler(
        store,
        initialState,
        evolve,
        decide,
    )
}
```

## Step 7: Use the Handler

```go
func main() {
    store := memory.NewEventStore()
    commandBus := cqrs.NewCommandBus(100, 1)
	
    cqrs.Register(commandBus, reserveseat.NewHandler(store))

    cmd := reserveseat.ReserveSeat{
        ScreeningID: uuid.New(),
        SeatNumber:  "A1",
        UserID:      uuid.New(),
    }
	
    if _, err := commandBus.Dispatch(ctx, cmd); err != nil {
        log.Printf("Command failed: %v", err)
		return
    }
}
```

## Handler Options

### Concurrency Control

Control optimistic locking with `WithStreamState`:

```go
handler := eventsourcing.NewCommandHandler(
    store, initialState, evolve, decide,
    eventsourcing.WithStreamState(eventsourcing.NoStream{}), // Must be new aggregate
)
```

Options:
- `Any{}` - No version check (default, last-write-wins)
- `NoStream{}` - Stream must not exist (for creation commands)
- `StreamExists{}` - Stream must exist
- `Revision(n)` - Stream must be at exact version n

### Automatic Retry on Conflicts

Handle concurrent updates with automatic retry:

```go
import "github.com/cenkalti/backoff/v4"

handler := eventsourcing.NewCommandHandler(
    store, initialState, evolve, decide,
    eventsourcing.WithRetryStrategy(
        backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 3),
    ),
)
```

When a `StreamRevisionConflictError` occurs, the handler:
1. Reloads the event stream
2. Re-evolves the state
3. Re-runs the decider
4. Attempts to save again

### Custom Metadata

Add metadata to events (e.g., user ID, correlation ID):

```go
handler := eventsourcing.NewCommandHandler(
    store, initialState, evolve, decide,
    eventsourcing.WithMetadataExtractor(func(ctx context.Context) map[string]any {
        return map[string]any{
            "user_id":        ctx.Value("user_id"),
            "correlation_id": ctx.Value("correlation_id"),
        }
    }),
)
```

### Custom Stream Naming

Override how stream names are derived:

```go
handler := eventsourcing.NewCommandHandler(
    store, initialState, evolve, decide,
    eventsourcing.WithStreamNamer(func(ctx context.Context, cmd eventsourcing.Command) string {
        tenant := ctx.Value("tenant").(string)
        return fmt.Sprintf("%s-screening-%s", tenant, cmd.AggregateID())
    }),
)
```

## Complete Example

```go
package reserveseat

import (
    "context"
    "fmt"

    "github.com/google/uuid"
    "github.com/terraskye/eventsourcing"
    "yourapp/events"
)

// Command
type ReserveSeat struct {
    ScreeningID uuid.UUID
    SeatNumber  string
    UserID      uuid.UUID
}

func (c ReserveSeat) AggregateID() string {
    return c.ScreeningID.String()
}

// State
type screeningState struct {
    ReservedSeats map[string]uuid.UUID // seat -> user who reserved it
    Capacity      int
}

var initialState = screeningState{
    ReservedSeats: make(map[string]uuid.UUID),
    Capacity:      100,
}

// Evolve
func evolve(state screeningState, env *eventsourcing.Envelope) screeningState {
    switch e := env.Event.(type) {
    case *events.SeatReserved:
        newSeats := make(map[string]uuid.UUID)
        for k, v := range state.ReservedSeats {
            newSeats[k] = v
        }
        newSeats[e.SeatNumber] = e.UserID
        return screeningState{
            ReservedSeats: newSeats,
            Capacity:      state.Capacity,
        }

    case *events.SeatReleased:
        newSeats := make(map[string]uuid.UUID)
        for k, v := range state.ReservedSeats {
            newSeats[k] = v
        }
        delete(newSeats, e.SeatNumber)
        return screeningState{
            ReservedSeats: newSeats,
            Capacity:      state.Capacity,
        }
    }
    return state
}

// Decide
func decide(state screeningState, cmd ReserveSeat) ([]eventsourcing.Event, error) {
    // Rule 1: Check capacity
    if len(state.ReservedSeats) >= state.Capacity {
        return nil, fmt.Errorf("screening is fully booked")
    }

    // Rule 2: Check if seat is available
    if existingUser, taken := state.ReservedSeats[cmd.SeatNumber]; taken {
        // Idempotency: same user reserving same seat is OK
        if existingUser == cmd.UserID {
            return nil, nil // No new events needed
        }
        return nil, fmt.Errorf("seat %s is already reserved", cmd.SeatNumber)
    }

    return []eventsourcing.Event{
        &events.SeatReserved{
            ScreeningID: cmd.ScreeningID,
            SeatNumber:  cmd.SeatNumber,
            UserID:      cmd.UserID,
        },
    }, nil
}

// Handler factory
func NewHandler(store eventsourcing.EventStore) eventsourcing.CommandHandler[ReserveSeat] {
    return eventsourcing.NewCommandHandler(
        store,
        initialState,
        evolve,
        decide,
    )
}
```

## Testing

Test evolvers and deciders independently:

```go
func TestEvolve_SeatReserved(t *testing.T) {
    state := initialState
    env := &eventsourcing.Envelope{
        Event: &events.SeatReserved{
            ScreeningID: uuid.New(),
            SeatNumber:  "A1",
            UserID:      uuid.New(),
        },
    }

    newState := evolve(state, env)

    assert.True(t, newState.ReservedSeats["A1"] != uuid.Nil)
}

func TestDecide_SeatAlreadyReserved(t *testing.T) {
    state := screeningState{
        ReservedSeats: map[string]uuid.UUID{"A1": uuid.New()},
    }
    cmd := ReserveSeat{
        ScreeningID: uuid.New(),
        SeatNumber:  "A1",
        UserID:      uuid.New(),
    }

    events, err := decide(state, cmd)

    assert.Error(t, err)
    assert.Nil(t, events)
    assert.Contains(t, err.Error(), "already reserved")
}

func TestDecide_Success(t *testing.T) {
    state := initialState
    cmd := ReserveSeat{
        ScreeningID: uuid.New(),
        SeatNumber:  "A1",
        UserID:      uuid.New(),
    }

    events, err := decide(state, cmd)

    assert.NoError(t, err)
    assert.Len(t, events, 1)
    assert.IsType(t, &events.SeatReserved{}, events[0])
}
```

## Summary

| Component | Responsibility | Pure? | Can Fail? |
|-----------|---------------|-------|-----------|
| Command | Express user intent | Yes | No |
| State | Data needed for decisions | Yes | No |
| Evolve | Apply event to state | Yes | No |
| Decide | Business rules → events | Yes | Yes |
| Handler | Wire everything together | No | Yes |

The Evolve + Decide pattern separates concerns:
- **Evolve** answers: "What is the current state?"
- **Decide** answers: "What should happen next?"

This separation makes testing easy, business logic explicit, and event sourcing natural.
