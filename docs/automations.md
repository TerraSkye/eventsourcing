# Implementing Automations

This guide explains how to implement automations in terraskye/eventsourcing.

## Overview

An **Automation** is a processor that:
1. Is triggered by an **event** or a **schedule** (e.g., cronjob)
2. Reads from a **read model** to make decisions
3. Produces a **command** as output

```
┌─────────────┐      ┌────────────┐      ┌─────────────┐      ┌─────────────┐
│   Trigger   │─────▶│ Automation │─────▶│   Command   │─────▶│ CommandBus  │
│ Event/Cron  │      │            │      │             │      │             │
└─────────────┘      └─────┬──────┘      └─────────────┘      └─────────────┘
                           │
                           ▼
                    ┌─────────────┐
                    │  Read Model │
                    │   (Query)   │
                    └─────────────┘
```

Automations enable reactive workflows where domain events or time-based schedules trigger business processes.

## Event-Triggered Automation

React to domain events by querying state and dispatching commands.

### Example: Auto-Archive Completed Orders

When an order is completed, archive it after checking it meets the criteria.

```go
package autoarchive

import (
    "context"
    "log"

    "github.com/terraskye/eventsourcing"
    "yourapp/events"
    "yourapp/slices/archiveorder"
    "yourapp/slices/orderdetails"
)

type Automation struct {
    commandBus   eventsourcing.Dispatcher
    orderDetails *orderdetails.QueryHandler
}

func NewAutomation(
    commandBus eventsourcing.Dispatcher,
    orderDetails *orderdetails.QueryHandler,
) *Automation {
    return &Automation{
        commandBus:   commandBus,
        orderDetails: orderDetails,
    }
}

// OnOrderCompleted is triggered when an order is completed
func (a *Automation) OnOrderCompleted(ctx context.Context, ev *events.OrderCompleted) error {
    // 1. Query the read model
    order, err := a.orderDetails.HandleQuery(ctx, orderdetails.Query{
        OrderID: ev.OrderID,
    })
    if err != nil {
        return err
    }

    // 2. Apply business logic
    if !order.IsPaid {
        log.Printf("Order %s not paid, skipping archive", ev.OrderID)
        return nil
    }

    if order.TotalAmount < 1000 {
        log.Printf("Order %s below threshold, skipping archive", ev.OrderID)
        return nil
    }

    // 3. Dispatch command
    cmd := archiveorder.ArchiveOrder{
        OrderID: ev.OrderID,
        Reason:  "auto-archived after completion",
    }

    _, err = a.commandBus.Dispatch(ctx, cmd)
    return err
}

// CreateEventHandler returns the handler for event bus subscription
func (a *Automation) CreateEventHandler() eventsourcing.EventHandler {
    return eventsourcing.NewEventGroupProcessor(
        eventsourcing.OnEvent(a.OnOrderCompleted),
    )
}
```

### Wiring Event-Triggered Automation

```go
func main() {
    store := memorystore.NewEventStore()
    commandBus := eventsourcing.NewCommandBus(100, 4)
    eventBus := memory.NewEventBus(100)

    // Register command handler
    eventsourcing.Register(commandBus, archiveorder.NewHandler(store))

    // Create query handler for read model
    orderDetails := orderdetails.NewQueryHandler(store)

    // Create and subscribe automation
    automation := autoarchive.NewAutomation(commandBus, orderDetails)
    eventBus.Subscribe(
        context.Background(),
        "auto-archive-automation",
        automation.CreateEventHandler(),
    )

    // ...
}
```

## Schedule-Triggered Automation

Run automations on a schedule using a ticker or cron library.

### Example: Daily Reminder for Abandoned Carts

```go
package abandonedcarts

import (
    "context"
    "log"
    "time"

    "github.com/terraskye/eventsourcing"
    "yourapp/slices/sendreminder"
    "yourapp/slices/stalecarts"
)

type Automation struct {
    commandBus eventsourcing.Dispatcher
    staleCarts *stalecarts.QueryHandler
}

func NewAutomation(
    commandBus eventsourcing.Dispatcher,
    staleCarts *stalecarts.QueryHandler,
) *Automation {
    return &Automation{
        commandBus: commandBus,
        staleCarts: staleCarts,
    }
}

// Run executes the automation logic
func (a *Automation) Run(ctx context.Context) error {
    // 1. Query the read model for stale carts
    carts, err := a.staleCarts.HandleQuery(ctx, stalecarts.Query{
        StaleSince: time.Now().Add(-24 * time.Hour),
        Limit:      100,
    })
    if err != nil {
        return err
    }

    // 2. Process each cart
    for _, cart := range carts.Items {
        // Apply business logic
        if cart.RemindersSent >= 3 {
            continue // Already sent max reminders
        }

        // 3. Dispatch command
        cmd := sendreminder.SendReminder{
            CartID: cart.CartID,
            UserID: cart.UserID,
            Type:   "abandoned_cart",
        }

        if _, err := a.commandBus.Dispatch(ctx, cmd); err != nil {
            log.Printf("Failed to send reminder for cart %s: %v", cart.CartID, err)
            // Continue processing other carts
        }
    }

    return nil
}

// Start begins the scheduled automation
func (a *Automation) Start(ctx context.Context) {
    ticker := time.NewTicker(1 * time.Hour)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            if err := a.Run(ctx); err != nil {
                log.Printf("Abandoned cart automation failed: %v", err)
            }
        }
    }
}
```

### Wiring Schedule-Triggered Automation

```go
func main() {
    store := memorystore.NewEventStore()
    commandBus := eventsourcing.NewCommandBus(100, 4)

    // Register command handler
    eventsourcing.Register(commandBus, sendreminder.NewHandler(store))

    // Create query handler
    staleCarts := stalecarts.NewQueryHandler(db)

    // Start automation in background
    automation := abandonedcarts.NewAutomation(commandBus, staleCarts)

    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    go automation.Start(ctx)

    // ...
}
```

## Combining Event and Schedule Triggers

Some automations need both triggers. For example, process payments immediately on event, but also retry failed ones on a schedule.

```go
package processpayment

import (
    "context"
    "time"

    "github.com/terraskye/eventsourcing"
    "yourapp/events"
    "yourapp/slices/chargepayment"
    "yourapp/slices/pendingpayments"
)

type Automation struct {
    commandBus      eventsourcing.Dispatcher
    pendingPayments *pendingpayments.QueryHandler
}

func NewAutomation(
    commandBus eventsourcing.Dispatcher,
    pendingPayments *pendingpayments.QueryHandler,
) *Automation {
    return &Automation{
        commandBus:      commandBus,
        pendingPayments: pendingPayments,
    }
}

// OnOrderPlaced handles immediate payment processing
func (a *Automation) OnOrderPlaced(ctx context.Context, ev *events.OrderPlaced) error {
    return a.processPayment(ctx, ev.OrderID)
}

// RetryPending processes failed payments on schedule
func (a *Automation) RetryPending(ctx context.Context) error {
    pending, err := a.pendingPayments.HandleQuery(ctx, pendingpayments.Query{
        Status:    "failed",
        MaxAge:    24 * time.Hour,
        MaxRetries: 3,
    })
    if err != nil {
        return err
    }

    for _, p := range pending.Items {
        _ = a.processPayment(ctx, p.OrderID)
    }
    return nil
}

func (a *Automation) processPayment(ctx context.Context, orderID uuid.UUID) error {
    // Query current state
    payment, err := a.pendingPayments.HandleQuery(ctx, pendingpayments.Query{
        OrderID: orderID,
    })
    if err != nil {
        return err
    }

    if payment.Status == "completed" {
        return nil // Already processed
    }

    // Dispatch command
    cmd := chargepayment.ChargePayment{
        OrderID: orderID,
        Amount:  payment.Amount,
    }

    _, err = a.commandBus.Dispatch(ctx, cmd)
    return err
}

// CreateEventHandler for event bus subscription
func (a *Automation) CreateEventHandler() eventsourcing.EventHandler {
    return eventsourcing.NewEventGroupProcessor(
        eventsourcing.OnEvent(a.OnOrderPlaced),
    )
}

// StartScheduler for retry logic
func (a *Automation) StartScheduler(ctx context.Context) {
    ticker := time.NewTicker(15 * time.Minute)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            a.RetryPending(ctx)
        }
    }
}
```

## Project Structure

Following vertical slice architecture, place automations in a dedicated directory:

```
automations/
  ├── auto_archive_orders/
  │   └── automation.go
  ├── abandoned_cart_reminders/
  │   └── automation.go
  └── process_payments/
      └── automation.go
```

## Testing Automations

Test automations by mocking the query handler and command bus:

```go
func TestAutoArchive_OnOrderCompleted(t *testing.T) {
    // Mock command bus
    var dispatchedCmd archiveorder.ArchiveOrder
    mockBus := &mockDispatcher{
        dispatchFn: func(ctx context.Context, cmd eventsourcing.Command) (eventsourcing.AppendResult, error) {
            dispatchedCmd = cmd.(archiveorder.ArchiveOrder)
            return eventsourcing.AppendResult{Successful: true}, nil
        },
    }

    // Mock query handler that returns a paid order
    mockQuery := &mockOrderDetails{
        order: &orderdetails.Order{
            OrderID: uuid.New(),
            IsPaid:  true,
            TotalAmount: 5000,
        },
    }

    automation := autoarchive.NewAutomation(mockBus, mockQuery)

    // Trigger
    err := automation.OnOrderCompleted(context.Background(), &events.OrderCompleted{
        OrderID: mockQuery.order.OrderID,
    })

    assert.NoError(t, err)
    assert.Equal(t, mockQuery.order.OrderID, dispatchedCmd.OrderID)
}

func TestAutoArchive_SkipsUnpaidOrders(t *testing.T) {
    dispatched := false
    mockBus := &mockDispatcher{
        dispatchFn: func(ctx context.Context, cmd eventsourcing.Command) (eventsourcing.AppendResult, error) {
            dispatched = true
            return eventsourcing.AppendResult{Successful: true}, nil
        },
    }

    mockQuery := &mockOrderDetails{
        order: &orderdetails.Order{
            IsPaid: false, // Not paid
        },
    }

    automation := autoarchive.NewAutomation(mockBus, mockQuery)
    err := automation.OnOrderCompleted(context.Background(), &events.OrderCompleted{})

    assert.NoError(t, err)
    assert.False(t, dispatched) // Should not dispatch command
}
```

## Summary

| Trigger | Use Case | Implementation |
|---------|----------|----------------|
| Event | Immediate reactions | Subscribe to EventBus with `OnEvent[T]` |
| Schedule | Periodic batch processing | Use `time.Ticker` or cron library |
| Both | Immediate + retry logic | Combine event handler and scheduler |

Automation pattern:
1. **Trigger** - Event or schedule activates the automation
2. **Query** - Read from a read model to get current state
3. **Decide** - Apply business logic to determine if action is needed
4. **Command** - Dispatch command to make state changes
