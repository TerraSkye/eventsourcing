# Implementing Projectors

This guide explains how to implement projectors (also called projections or read models) in terraskye/eventsourcing.

## Overview

In event sourcing, **projectors** transform event streams into read-optimized views. They subscribe to events and build/update read models that are optimized for querying.

There are two main strategies:
1. **Live Read Models** - Rebuild from events on each query
2. **Cached Projections** - Maintain state updated in real-time by events

## Core Building Blocks

### EventHandler Interface

The foundation for all event handling:

```go
type EventHandler interface {
    Handle(ctx context.Context, event Event) error
}
```

### Type-Safe Event Handlers with OnEvent[T]

Create handlers that only receive specific event types:

```go
handler := eventsourcing.OnEvent(func(ctx context.Context, ev *ItemAdded) error {
    // Only called for ItemAdded events
    fmt.Printf("Item %s added to cart %s\n", ev.ItemID, ev.CartID)
    return nil
})
```

### EventGroupProcessor

Group multiple typed handlers together. Events are automatically routed to the correct handler based on event type:

```go
processor := eventsourcing.NewEventGroupProcessor(
    eventsourcing.OnEvent(projector.OnCartCreated),
    eventsourcing.OnEvent(projector.OnItemAdded),
    eventsourcing.OnEvent(projector.OnItemRemoved),
)
```

Key behaviors:
- Returns `ErrSkippedEvent` for events with no registered handler
- Panics on duplicate handlers for the same event type
- `StreamFilter()` returns sorted list of handled event types (useful for filtered subscriptions)

## Strategy 1: Live Read Model

Rebuild the read model from events on every query. Simple to implement, always consistent, but slower for large event streams.

```go
package cartitems

import (
    "context"
    "github.com/google/uuid"
    "github.com/terraskye/eventsourcing"
    "yourapp/events"
)

// Query definition
type Query struct {
    CartID uuid.UUID
}

func (q Query) ID() []byte {
    return []byte(q.CartID.String())
}

// Read model structure
type CartItems struct {
    CartID uuid.UUID
    Items  []Item
    Total  int64
}

type Item struct {
    ItemID    uuid.UUID
    ProductID uuid.UUID
    Quantity  int
    Price     int64
}

// QueryHandler rebuilds read model on each query
type QueryHandler struct {
    store eventsourcing.EventStore
}

func NewQueryHandler(store eventsourcing.EventStore) *QueryHandler {
    return &QueryHandler{store: store}
}

func (h *QueryHandler) HandleQuery(ctx context.Context, q Query) (*CartItems, error) {
    iter, err := h.store.LoadStream(ctx, q.CartID.String())
    if err != nil {
        return nil, err
    }

    model := &CartItems{
        CartID: q.CartID,
        Items:  make([]Item, 0),
    }

    // Replay all events to build current state
    for iter.Next(ctx) {
        model = applyEvent(model, iter.Value())
    }

    return model, iter.Err()
}

func applyEvent(model *CartItems, env *eventsourcing.Envelope) *CartItems {
    switch e := env.Event.(type) {
    case *events.ItemAdded:
        model.Items = append(model.Items, Item{
            ItemID:    e.ItemID,
            ProductID: e.ProductID,
            Quantity:  e.Quantity,
            Price:     e.Price,
        })
        model.Total += e.Price * int64(e.Quantity)

    case *events.ItemRemoved:
        for i, item := range model.Items {
            if item.ItemID == e.ItemID {
                model.Total -= item.Price * int64(item.Quantity)
                model.Items = append(model.Items[:i], model.Items[i+1:]...)
                break
            }
        }
    }
    return model
}
```

**When to use:**
- Small event streams (< 1000 events per aggregate)
- Strong consistency requirements
- Simple read models
- Development/testing

## Strategy 2: Cached Projection

Maintain an in-memory or persisted cache updated in real-time by subscribing to the event bus.

```go
package cartsummary

import (
    "context"
    "sync"
    "github.com/google/uuid"
    "github.com/terraskye/eventsourcing"
    "yourapp/events"
)

type CartSummary struct {
    CartID     uuid.UUID
    ItemCount  int
    TotalPrice int64
}

type Projector struct {
    store  eventsourcing.EventStore
    cache  map[uuid.UUID]*CartSummary
    mu     sync.RWMutex
}

func NewProjector(store eventsourcing.EventStore) *Projector {
    return &Projector{
        store: store,
        cache: make(map[uuid.UUID]*CartSummary),
    }
}

// Event handlers for real-time updates
func (p *Projector) OnItemAdded(ctx context.Context, ev *events.ItemAdded) error {
    p.mu.Lock()
    defer p.mu.Unlock()

    summary, exists := p.cache[ev.CartID]
    if !exists {
        summary = &CartSummary{CartID: ev.CartID}
        p.cache[ev.CartID] = summary
    }

    summary.ItemCount += ev.Quantity
    summary.TotalPrice += ev.Price * int64(ev.Quantity)
    return nil
}

func (p *Projector) OnItemRemoved(ctx context.Context, ev *events.ItemRemoved) error {
    p.mu.Lock()
    defer p.mu.Unlock()

    if summary, exists := p.cache[ev.CartID]; exists {
        summary.ItemCount -= ev.Quantity
        summary.TotalPrice -= ev.Price * int64(ev.Quantity)
    }
    return nil
}

// Query handler returns cached data
func (p *Projector) HandleQuery(ctx context.Context, q Query) (*CartSummary, error) {
    p.mu.RLock()
    summary, exists := p.cache[q.CartID]
    p.mu.RUnlock()

    if exists {
        return summary, nil
    }

    // Cache miss: rebuild from events
    return p.rebuildFromEvents(ctx, q.CartID)
}

func (p *Projector) rebuildFromEvents(ctx context.Context, cartID uuid.UUID) (*CartSummary, error) {
    iter, err := p.store.LoadStream(ctx, cartID.String())
    if err != nil {
        return nil, err
    }

    summary := &CartSummary{CartID: cartID}

    for iter.Next(ctx) {
        switch e := iter.Value().Event.(type) {
        case *events.ItemAdded:
            summary.ItemCount += e.Quantity
            summary.TotalPrice += e.Price * int64(e.Quantity)
        case *events.ItemRemoved:
            summary.ItemCount -= e.Quantity
            summary.TotalPrice -= e.Price * int64(e.Quantity)
        }
    }

    if err := iter.Err(); err != nil {
        return nil, err
    }

    // Store in cache
    p.mu.Lock()
    p.cache[cartID] = summary
    p.mu.Unlock()

    return summary, nil
}

// CreateEventHandler returns the handler for subscription
func (p *Projector) CreateEventHandler() eventsourcing.EventHandler {
    return eventsourcing.NewEventGroupProcessor(
        eventsourcing.OnEvent(p.OnItemAdded),
        eventsourcing.OnEvent(p.OnItemRemoved),
    )
}
```

**When to use:**
- Large event streams
- High query frequency
- Complex aggregations across multiple streams
- Eventually consistent reads are acceptable

## Strategy 3: Database-Persisted Projection

For production systems, persist projections to a database. This survives restarts, enables horizontal scaling, and allows complex queries.

### Database Schema

```sql
-- Read model table
CREATE TABLE cart_summaries (
    cart_id UUID PRIMARY KEY,
    item_count INT NOT NULL DEFAULT 0,
    total_price BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMP NOT NULL DEFAULT NOW()
);

-- Track projection position (which events have been processed)
CREATE TABLE projection_checkpoints (
    projection_name VARCHAR(255) PRIMARY KEY,
    last_global_version BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMP NOT NULL DEFAULT NOW()
);
```

### Complete Implementation

```go
package cartsummary

import (
    "context"
    "database/sql"
    "fmt"

    "github.com/google/uuid"
    "github.com/terraskye/eventsourcing"
    "yourapp/events"
)

const projectionName = "cart-summary"

type CartSummary struct {
    CartID     uuid.UUID
    ItemCount  int
    TotalPrice int64
}

type Projector struct {
    db    *sql.DB
    store eventsourcing.EventStore
}

func NewProjector(db *sql.DB, store eventsourcing.EventStore) *Projector {
    return &Projector{db: db, store: store}
}

// OnItemAdded handles ItemAdded events
func (p *Projector) OnItemAdded(ctx context.Context, ev *events.ItemAdded) error {
    globalVersion := eventsourcing.GlobalVersionFromContext(ctx)

    tx, err := p.db.BeginTx(ctx, nil)
    if err != nil {
        return fmt.Errorf("begin tx: %w", err)
    }
    defer tx.Rollback()

    // Check if already processed (idempotency)
    if processed, err := p.alreadyProcessed(ctx, tx, globalVersion); err != nil {
        return err
    } else if processed {
        return nil // Skip duplicate
    }

    // Upsert the cart summary
    _, err = tx.ExecContext(ctx, `
        INSERT INTO cart_summaries (cart_id, item_count, total_price, updated_at)
        VALUES ($1, $2, $3, NOW())
        ON CONFLICT (cart_id) DO UPDATE SET
            item_count = cart_summaries.item_count + $2,
            total_price = cart_summaries.total_price + $3,
            updated_at = NOW()
    `, ev.CartID, ev.Quantity, ev.Price*int64(ev.Quantity))
    if err != nil {
        return fmt.Errorf("upsert cart summary: %w", err)
    }

    // Update checkpoint
    if err := p.updateCheckpoint(ctx, tx, globalVersion); err != nil {
        return err
    }

    return tx.Commit()
}

// OnItemRemoved handles ItemRemoved events
func (p *Projector) OnItemRemoved(ctx context.Context, ev *events.ItemRemoved) error {
    globalVersion := eventsourcing.GlobalVersionFromContext(ctx)

    tx, err := p.db.BeginTx(ctx, nil)
    if err != nil {
        return fmt.Errorf("begin tx: %w", err)
    }
    defer tx.Rollback()

    if processed, err := p.alreadyProcessed(ctx, tx, globalVersion); err != nil {
        return err
    } else if processed {
        return nil
    }

    _, err = tx.ExecContext(ctx, `
        UPDATE cart_summaries SET
            item_count = item_count - $2,
            total_price = total_price - $3,
            updated_at = NOW()
        WHERE cart_id = $1
    `, ev.CartID, ev.Quantity, ev.Price*int64(ev.Quantity))
    if err != nil {
        return fmt.Errorf("update cart summary: %w", err)
    }

    if err := p.updateCheckpoint(ctx, tx, globalVersion); err != nil {
        return err
    }

    return tx.Commit()
}

// alreadyProcessed checks if this event was already handled
func (p *Projector) alreadyProcessed(ctx context.Context, tx *sql.Tx, globalVersion uint64) (bool, error) {
    var lastVersion uint64
    err := tx.QueryRowContext(ctx, `
        SELECT last_global_version FROM projection_checkpoints
        WHERE projection_name = $1
        FOR UPDATE
    `, projectionName).Scan(&lastVersion)

    if err == sql.ErrNoRows {
        // First event for this projection
        _, err = tx.ExecContext(ctx, `
            INSERT INTO projection_checkpoints (projection_name, last_global_version)
            VALUES ($1, 0)
        `, projectionName)
        return false, err
    }
    if err != nil {
        return false, fmt.Errorf("check checkpoint: %w", err)
    }

    return globalVersion <= lastVersion, nil
}

// updateCheckpoint stores the last processed position
func (p *Projector) updateCheckpoint(ctx context.Context, tx *sql.Tx, globalVersion uint64) error {
    _, err := tx.ExecContext(ctx, `
        UPDATE projection_checkpoints SET
            last_global_version = $2,
            updated_at = NOW()
        WHERE projection_name = $1
    `, projectionName, globalVersion)
    return err
}

// GetCheckpoint returns the last processed global version
func (p *Projector) GetCheckpoint(ctx context.Context) (uint64, error) {
    var version uint64
    err := p.db.QueryRowContext(ctx, `
        SELECT last_global_version FROM projection_checkpoints
        WHERE projection_name = $1
    `, projectionName).Scan(&version)

    if err == sql.ErrNoRows {
        return 0, nil
    }
    return version, err
}

// HandleQuery retrieves the projection from the database
func (p *Projector) HandleQuery(ctx context.Context, q Query) (*CartSummary, error) {
    var summary CartSummary
    err := p.db.QueryRowContext(ctx, `
        SELECT cart_id, item_count, total_price
        FROM cart_summaries
        WHERE cart_id = $1
    `, q.CartID).Scan(&summary.CartID, &summary.ItemCount, &summary.TotalPrice)

    if err == sql.ErrNoRows {
        return &CartSummary{CartID: q.CartID}, nil
    }
    if err != nil {
        return nil, fmt.Errorf("query cart summary: %w", err)
    }
    return &summary, nil
}

// CreateEventHandler returns the handler for subscription
func (p *Projector) CreateEventHandler() eventsourcing.EventHandler {
    return eventsourcing.NewEventGroupProcessor(
        eventsourcing.OnEvent(p.OnItemAdded),
        eventsourcing.OnEvent(p.OnItemRemoved),
    )
}
```

### Catch-Up on Startup

When the application starts, replay any events that were missed while offline:

```go
func (p *Projector) CatchUp(ctx context.Context) error {
    // Get last processed position
    lastVersion, err := p.GetCheckpoint(ctx)
    if err != nil {
        return fmt.Errorf("get checkpoint: %w", err)
    }

    // Load all events after that position
    iter, err := p.store.LoadAll(ctx, eventsourcing.Revision(lastVersion))
    if err != nil {
        return fmt.Errorf("load events: %w", err)
    }

    handler := p.CreateEventHandler()

    // Process each event
    for iter.Next(ctx) {
        env := iter.Value()
        ctx := eventsourcing.WithEnvelope(ctx, env)

        if err := handler.Handle(ctx, env.Event); err != nil {
            var skipped *eventsourcing.ErrSkippedEvent
            if !errors.As(err, &skipped) {
                return fmt.Errorf("handle event: %w", err)
            }
            // Skipped events are fine - just not relevant to this projection
        }
    }

    return iter.Err()
}
```

### Wiring It Together

```go
func main() {
    db, _ := sql.Open("postgres", "postgres://...")
    store := kurrentdb.NewEventStore(client)
    bus := kurrentdb.NewEventBus(client)

    projector := cartsummary.NewProjector(db, store)

    // Catch up on missed events before subscribing
    if err := projector.CatchUp(context.Background()); err != nil {
        log.Fatal(err)
    }

    // Subscribe for real-time updates
    ctx := context.Background()
    err := bus.Subscribe(ctx, "cart-summary-projector", projector.CreateEventHandler())
    if err != nil {
        log.Fatal(err)
    }

    // ...
}
```

### Key Concepts

**Idempotency**: Events may be delivered more than once (at-least-once delivery). Use the global version or event ID to detect and skip duplicates.

**Checkpointing**: Track the last processed position so you can resume from where you left off after a restart.

**Transactions**: Update the read model and checkpoint atomically to prevent inconsistencies.

**Catch-up**: On startup, replay events from the last checkpoint before subscribing to live events.

**When to use:**
- Production systems
- Data must survive restarts
- Multiple application instances (horizontal scaling)
- Complex SQL queries on read models

## Subscribing to the Event Bus

Register your projector to receive events:

```go
package main

import (
    "context"
    "github.com/terraskye/eventsourcing"
    "github.com/terraskye/eventsourcing/eventbus/memory"
    "github.com/terraskye/eventsourcing/eventstore/memory"
    "yourapp/slices/cartsummary"
)

func main() {
    store := memorystore.NewEventStore()
    bus := memorybus.NewEventBus(100) // buffer size

    // Create projector
    projector := cartsummary.NewProjector(store)

    // Subscribe to events
    ctx := context.Background()
    err := bus.Subscribe(ctx, "cart-summary-projector", projector.CreateEventHandler())
    if err != nil {
        panic(err)
    }

    // Handle errors from the bus
    go func() {
        for err := range bus.Errors() {
            log.Printf("Event bus error: %v", err)
        }
    }()

    // ... rest of application
}
```

### Filtered Subscriptions

Subscribe only to specific event types using `WithFilterEvents`:

```go
import "github.com/terraskye/eventsourcing/eventbus/memory"

handler := projector.CreateEventHandler()

// Get the list of events this handler cares about
processor := handler.(*eventsourcing.EventGroupProcessor)
filter := processor.StreamFilter() // ["ItemAdded", "ItemRemoved"]

// Subscribe with filter
err := bus.Subscribe(
    ctx,
    "cart-summary-projector",
    handler,
    memory.WithFilterEvents(filter),
)
```

## Accessing Event Metadata

Event handlers receive context with envelope metadata:

```go
func (p *Projector) OnItemAdded(ctx context.Context, ev *events.ItemAdded) error {
    // Access envelope metadata from context
    streamID := eventsourcing.StreamIDFromContext(ctx)
    eventID := eventsourcing.EventIDFromContext(ctx)
    version := eventsourcing.VersionFromContext(ctx)
    occurredAt := eventsourcing.OccurredAtFromContext(ctx)
    metadata := eventsourcing.MetadataFromContext(ctx)

    log.Printf("Processing event %s (version %d) from stream %s at %s",
        eventID, version, streamID, occurredAt)

    // ... handle event
    return nil
}
```

## Error Handling

### Skipped Events

When an event type has no handler, `ErrSkippedEvent` is returned:

```go
err := handler.Handle(ctx, someUnknownEvent)
var skipped *eventsourcing.ErrSkippedEvent
if errors.As(err, &skipped) {
    // Event was not handled - this is often expected
    log.Printf("Skipped event: %s", skipped.Event.EventType())
}
```

### Handler Errors

Errors from handlers are sent to the error channel:

```go
go func() {
    for err := range bus.Errors() {
        // Log, retry, alert, etc.
        log.Printf("Projector error: %v", err)
    }
}()
```

## Recommended Project Structure

Following vertical slice architecture:

```
slices/
  └── cart_summary/
      ├── query.go        # Query type definition
      ├── readmodel.go    # CartSummary struct
      ├── projector.go    # Projector with event handlers
      └── http.go         # Optional HTTP endpoint
```

## Testing Projectors

```go
func TestProjector_ItemAdded(t *testing.T) {
    store := memorystore.NewEventStore()
    projector := cartsummary.NewProjector(store)

    cartID := uuid.New()

    // Simulate event
    err := projector.OnItemAdded(context.Background(), &events.ItemAdded{
        CartID:   cartID,
        Quantity: 2,
        Price:    1000,
    })
    require.NoError(t, err)

    // Query the projection
    summary, err := projector.HandleQuery(context.Background(), cartsummary.Query{CartID: cartID})
    require.NoError(t, err)

    assert.Equal(t, 2, summary.ItemCount)
    assert.Equal(t, int64(2000), summary.TotalPrice)
}
```

## Summary

| Approach | Consistency | Performance | Complexity | Use When |
|----------|-------------|-------------|------------|----------|
| Live Read Model | Strong | O(n) per query | Low | Small streams, strong consistency needed |
| Cached Projection (Memory) | Eventual | O(1) per query | Medium | Large streams, single instance |
| Cached Projection (Database) | Eventual | O(1) per query | High | Production, horizontal scaling, persistence |

Key components:
- `OnEvent[T]` - Type-safe single-event handlers
- `NewEventGroupProcessor` - Routes events to correct handlers
- `EventBus.Subscribe` - Receive events in real-time
- Context helpers - Access event metadata (stream ID, version, timestamp)
