# Architecture Patterns - Vertical Slice Architecture

This guide explains the **Vertical Slice Architecture** pattern recommended for terraskye/eventsourcing applications.

## Overview

Vertical Slice Architecture organizes code by **feature** rather than by **technical layer**. Each slice contains everything needed for a specific use case, from HTTP handling to business logic to data access.

## Why Vertical Slices?

Traditional layered architecture:
```
❌ Organized by technical concern
controllers/
  ├── cart_controller.go
  ├── product_controller.go
services/
  ├── cart_service.go
  ├── product_service.go
repositories/
  ├── cart_repository.go
  ├── product_repository.go
```

**Problems**:
- Changes span multiple directories
- Hard to see complete feature
- Artificial layers with unclear boundaries
- Shared abstractions become coupling points

Vertical slice architecture:
```
✅ Organized by feature/use case
slices/
  ├── add_item/
  │   ├── command.go
  │   └── http.go
  ├── remove_item/
  │   ├── command.go
  │   └── http.go
  └── cartview/
      ├── query.go
      ├── readmodel.go
      └── projector.go
```

**Benefits**:
- ✅ Everything for a feature in one place
- ✅ Changes are localized
- ✅ Easy to understand complete flow
- ✅ Easy to add/remove features
- ✅ Teams can own slices
- ✅ Natural fit for event sourcing (commands/queries are slices)

## Recommended Structure

```
your-app/
├── cmd/
│   └── api/
│       └── main.go              # Application entry point
│
├── events/                      # Domain events (shared)
│   ├── cart_cleared.go
│   ├── cart_created.go
│   ├── cart_submitted.go
│   ├── inventory_changed.go
│   ├── item_added.go
│   ├── item_archived.go
│   ├── item_removed.go
│   └── price_changed.go
│
├── slices/                      # Feature slices
│   ├── add_item/               # Command slice
│   │   ├── command.go          # Command + handler + business logic
│   │   └── http.go             # HTTP endpoint
│   │
│   ├── remove_item/            # Command slice
│   │   ├── command.go
│   │   └── http.go
│   │
│   ├── cartitems/              # Query slice (live read model)
│   │   ├── query.go            # Query definition
│   │   ├── readmodel.go        # Read model structure
│   │   └── http.go             # HTTP endpoint
│   │
│   └── cartwithproducts/       # Query slice (cached projection)
│       ├── query.go
│       ├── readmodel.go
│       ├── projector.go        # Projection logic + event handlers
│       └── http.go             # HTTP endpoint (optional)
│
├── screens/                     # Actor-specific compositions
│   ├── backoffice_add_item_to_cart/
│   │   ├── service.go          # Business orchestration
│   │   ├── endpoint.go         # Request/response mapping
│   │   └── http.go             # HTTP handler
│   │
│   └── storefront_add_item_to_cart/
│       ├── service.go
│       ├── endpoint.go
│       └── http.go
│
├── processors/                  # Background processors/sagas
│   └── archive_items/
│       └── processor.go        # Event-driven background work
│
├── shared/                      # Truly shared code
│   ├── domain/
│   │   └── money.go            # Value objects
│   └── middleware/
│       └── logging.go          # Cross-cutting concerns
│
└── go.mod
```

## Core Concepts

### 1. Events (Shared)

Events are the **only** shared code between slices. They represent facts about what happened.

```go
// events/item_added.go
package events

import (
    "time"
    "github.com/google/uuid"
)

type ItemAdded struct {
    CartID      uuid.UUID `json:"cart_id"`
    ItemID      uuid.UUID `json:"item_id"`
    ProductID   uuid.UUID `json:"product_id"`
    Quantity    int       `json:"quantity"`
    Price       int64     `json:"price"`
    AddedAt     time.Time `json:"added_at"`
}

func (e *ItemAdded) AggregateID() string {
    return e.CartID.String()
}
```

**Key Points**:
- Events are immutable facts
- Shared across all slices
- Never contain business logic
- Implement `Event` interface

### 2. Slices (Features)

Each slice is a **complete vertical feature**. There are two types:

#### Command Slices (State Changes)

Structure: `command.go + http.go`

```go
// slices/add_item/command.go
package additem

import (
    "context"
    "fmt"
    "github.com/google/uuid"
    "github.com/terraskye/eventsourcing"
    "yourapp/events"
)

// Command
type AddItem struct {
    CartID    uuid.UUID
    ProductID uuid.UUID
    Quantity  int
    Price     int64
}

func (c AddItem) AggregateID() string {
    return c.CartID.String()
}

// State
type cartState struct {
    Items        map[uuid.UUID]int
    IsSubmitted  bool
}

var initialState = cartState{
    Items: make(map[uuid.UUID]int),
}

// Evolve
func evolve(state cartState, envelope *eventsourcing.Envelope) cartState {
    switch e := envelope.Event.(type) {
    case *events.ItemAdded:
        newItems := make(map[uuid.UUID]int)
        for k, v := range state.Items {
            newItems[k] = v
        }
        newItems[e.ProductID] += e.Quantity
        
        return cartState{
            Items:       newItems,
            IsSubmitted: state.IsSubmitted,
        }
    
    case *events.CartSubmitted:
        return cartState{
            Items:       state.Items,
            IsSubmitted: true,
        }
    }
    
    return state
}

// Decide (business rules)
func decide(state cartState, cmd AddItem) ([]eventsourcing.Event, error) {
    // Business rule: Cannot add to submitted cart
    if state.IsSubmitted {
        return nil, fmt.Errorf("cannot add items to submitted cart")
    }
    
    // Business rule: Quantity must be positive
    if cmd.Quantity <= 0 {
        return nil, fmt.Errorf("quantity must be positive")
    }
    
    // Business rule: Max 100 per product
    if state.Items[cmd.ProductID] + cmd.Quantity > 100 {
        return nil, fmt.Errorf("cannot exceed 100 units per product")
    }
    
    return []eventsourcing.Event{
        &events.ItemAdded{
            CartID:    cmd.CartID,
            ItemID:    uuid.New(),
            ProductID: cmd.ProductID,
            Quantity:  cmd.Quantity,
            Price:     cmd.Price,
            AddedAt:   time.Now(),
        },
    }, nil
}

// Handler factory
func NewHandler(store eventsourcing.EventStore) eventsourcing.CommandHandler[AddItem] {
    return eventsourcing.NewCommandHandler(
        store,
        initialState,
        evolve,
        decide,
    )
}
```

```go
// slices/add_item/http.go
package additem

import (
    "encoding/json"
    "net/http"
    "github.com/google/uuid"
    "github.com/gorilla/mux"
)

type HTTPHandler struct {
    handler eventsourcing.CommandHandler[AddItem]
}

func NewHTTPHandler(handler eventsourcing.CommandHandler[AddItem]) *HTTPHandler {
    return &HTTPHandler{handler: handler}
}

type Request struct {
    ProductID string  `json:"product_id"`
    Quantity  int     `json:"quantity"`
    Price     float64 `json:"price"`
}

func (h *HTTPHandler) Handle(w http.ResponseWriter, r *http.Request) {
    vars := mux.Vars(r)
    cartID, _ := uuid.Parse(vars["cartID"])
    
    var req Request
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }
    
    productID, _ := uuid.Parse(req.ProductID)
    
    cmd := AddItem{
        CartID:    cartID,
        ProductID: productID,
        Quantity:  req.Quantity,
        Price:     int64(req.Price * 100),
    }
    
    result, err := h.handler(r.Context(), cmd)
    if err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }
    
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(map[string]interface{}{
        "success": result.Successful,
        "version": result.NextExpectedVersion,
    })
}

func (h *HTTPHandler) RegisterRoutes(router *mux.Router) {
    router.HandleFunc("/carts/{cartID}/items", h.Handle).Methods("POST")
}
```

#### Query Slices (State Views)

**Option A: Live Read Model** (rebuilds on every query)

Structure: `query.go + readmodel.go + http.go`

```go
// slices/cartitems/query.go
package cartitems

import (
    "github.com/google/uuid"
)

type Query struct {
    CartID uuid.UUID
}

func (q Query) ID() []byte {
    return []byte(q.CartID.String())
}
```

```go
// slices/cartitems/readmodel.go
package cartitems

import (
    "context"
    "fmt"
    "github.com/google/uuid"
    "github.com/terraskye/eventsourcing"
    "yourapp/events"
)

type CartItems struct {
    CartID uuid.UUID `json:"cart_id"`
    Items  []Item    `json:"items"`
    Total  int64     `json:"total"`
}

type Item struct {
    ItemID    uuid.UUID `json:"item_id"`
    ProductID uuid.UUID `json:"product_id"`
    Quantity  int       `json:"quantity"`
    Price     int64     `json:"price"`
}

// Evolve builds the read model from events
func evolve(state *CartItems, envelope *eventsourcing.Envelope) *CartItems {
    switch e := envelope.Event.(type) {
    case *events.ItemAdded:
        // Add item
        state.Items = append(state.Items, Item{
            ItemID:    e.ItemID,
            ProductID: e.ProductID,
            Quantity:  e.Quantity,
            Price:     e.Price,
        })
        state.Total += e.Price * int64(e.Quantity)
        
    case *events.ItemRemoved:
        // Remove item
        for i, item := range state.Items {
            if item.ItemID == e.ItemID {
                state.Total -= item.Price * int64(item.Quantity)
                state.Items = append(state.Items[:i], state.Items[i+1:]...)
                break
            }
        }
        
    case *events.CartCleared:
        state.Items = []Item{}
        state.Total = 0
    }
    
    return state
}

// QueryHandler builds read model on demand
type QueryHandler struct {
    repository eventsourcing.EventStore
}

func NewQueryHandler(repository eventsourcing.EventStore) *QueryHandler {
    return &QueryHandler{repository: repository}
}

func (h *QueryHandler) HandleQuery(ctx context.Context, qry Query) (*CartItems, error) {
    iter, err := h.repository.LoadStream(ctx, qry.CartID.String())
    if err != nil {
        return nil, fmt.Errorf("failed to load stream: %w", err)
    }
    
    model := &CartItems{
        CartID: qry.CartID,
        Items:  make([]Item, 0),
    }
    
    for iter.Next(ctx) {
        model = evolve(model, iter.Value())
    }
    
    if err := iter.Err(); err != nil {
        return nil, err
    }
    
    return model, nil
}
```

```go
// slices/cartitems/http.go
package cartitems

import (
    "encoding/json"
    "net/http"
    "github.com/google/uuid"
    "github.com/gorilla/mux"
)

type HTTPHandler struct {
    queryHandler *QueryHandler
}

func NewHTTPHandler(queryHandler *QueryHandler) *HTTPHandler {
    return &HTTPHandler{queryHandler: queryHandler}
}

func (h *HTTPHandler) Handle(w http.ResponseWriter, r *http.Request) {
    vars := mux.Vars(r)
    cartID, _ := uuid.Parse(vars["cartID"])
    
    query := Query{CartID: cartID}
    
    result, err := h.queryHandler.HandleQuery(r.Context(), query)
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(result)
}

func (h *HTTPHandler) RegisterRoutes(router *mux.Router) {
    router.HandleFunc("/carts/{cartID}/items", h.Handle).Methods("GET")
}
```

**Option B: Cached Projection** (pre-built read model)

Structure: `query.go + readmodel.go + projector.go`

```go
// slices/cartwithproducts/projector.go
package cartwithproducts

import (
    "context"
    "sync"
    "github.com/google/uuid"
    "github.com/terraskye/eventsourcing"
    "yourapp/events"
)

type Projector struct {
    eventStore eventsourcing.EventStore
    views      map[uuid.UUID]*CartWithProducts
    mu         sync.RWMutex
}

func NewProjector(eventStore eventsourcing.EventStore) *Projector {
    return &Projector{
        eventStore: eventStore,
        views:      make(map[uuid.UUID]*CartWithProducts),
    }
}

// HandleQuery implements QueryHandler
func (p *Projector) HandleQuery(ctx context.Context, query Query) (*CartWithProducts, error) {
    // Check cache
    p.mu.RLock()
    if view, exists := p.views[query.CartID]; exists {
        p.mu.RUnlock()
        return view, nil
    }
    p.mu.RUnlock()
    
    // Build from events
    view, err := p.buildFromEvents(ctx, query.CartID)
    if err != nil {
        return nil, err
    }
    
    // Cache
    p.mu.Lock()
    p.views[query.CartID] = view
    p.mu.Unlock()
    
    return view, nil
}

func (p *Projector) buildFromEvents(ctx context.Context, cartID uuid.UUID) (*CartWithProducts, error) {
    iter, _ := p.eventStore.LoadStream(ctx, cartID.String())
    
    view := &CartWithProducts{CartID: cartID, Products: make([]Product, 0)}
    
    for iter.Next(ctx) {
        p.applyEvent(view, iter.Value().Event)
    }
    
    return view, nil
}

func (p *Projector) applyEvent(view *CartWithProducts, event eventsourcing.Event) {
    switch e := event.(type) {
    case *events.ItemAdded:
        view.Products = append(view.Products, Product{
            ProductID: e.ProductID,
            Quantity:  e.Quantity,
            Price:     e.Price,
        })
    }
}

// Event handlers for real-time updates
func (p *Projector) OnItemAdded(ctx context.Context, event *events.ItemAdded) error {
    p.mu.Lock()
    defer p.mu.Unlock()
    
    if view, exists := p.views[event.CartID]; exists {
        p.applyEvent(view, event)
    }
    
    return nil
}

func (p *Projector) CreateEventHandlers() []eventsourcing.EventHandler {
    return []eventsourcing.EventHandler{
        eventsourcing.OnEvent(p.OnItemAdded),
    }
}
```

### 3. Screens (Actor-Specific Compositions)

Screens compose multiple slices for a specific actor (user type) or use case.

Structure: `service.go + endpoint.go + http.go`

```go
// screens/storefront_add_item_to_cart/service.go
package storefrontadditem

import (
    "context"
    "fmt"
    "github.com/google/uuid"
    "yourapp/slices/additem"
    "yourapp/slices/inventories"
)

// Service orchestrates multiple slices
type Service struct {
    addItemHandler     eventsourcing.CommandHandler[additem.AddItem]
    inventoryQuery     *inventories.QueryHandler
}

func NewService(
    addItemHandler eventsourcing.CommandHandler[additem.AddItem],
    inventoryQuery *inventories.QueryHandler,
) *Service {
    return &Service{
        addItemHandler: addItemHandler,
        inventoryQuery: inventoryQuery,
    }
}

func (s *Service) AddItemToCart(ctx context.Context, cartID, productID uuid.UUID, quantity int) error {
    // Check inventory first
    inv, err := s.inventoryQuery.HandleQuery(ctx, inventories.Query{ProductID: productID})
    if err != nil {
        return err
    }
    
    if inv.Available < quantity {
        return fmt.Errorf("insufficient inventory: have %d, need %d", inv.Available, quantity)
    }
    
    // Add to cart
    cmd := additem.AddItem{
        CartID:    cartID,
        ProductID: productID,
        Quantity:  quantity,
        Price:     inv.Price,
    }
    
    _, err = s.addItemHandler(ctx, cmd)
    return err
}
```

```go
// screens/storefront_add_item_to_cart/endpoint.go
package storefrontadditem

import (
    "context"
    "github.com/google/uuid"
)

type Request struct {
    CartID    uuid.UUID
    ProductID uuid.UUID
    Quantity  int
}

type Response struct {
    Success bool   `json:"success"`
    Error   string `json:"error,omitempty"`
}

func MakeEndpoint(svc *Service) func(context.Context, Request) (Response, error) {
    return func(ctx context.Context, req Request) (Response, error) {
        err := svc.AddItemToCart(ctx, req.CartID, req.ProductID, req.Quantity)
        if err != nil {
            return Response{Success: false, Error: err.Error()}, nil
        }
        
        return Response{Success: true}, nil
    }
}
```

```go
// screens/storefront_add_item_to_cart/http.go
package storefrontadditem

import (
    "encoding/json"
    "net/http"
    "github.com/google/uuid"
    "github.com/gorilla/mux"
)

type HTTPHandler struct {
    endpoint func(context.Context, Request) (Response, error)
}

func NewHTTPHandler(endpoint func(context.Context, Request) (Response, error)) *HTTPHandler {
    return &HTTPHandler{endpoint: endpoint}
}

func (h *HTTPHandler) Handle(w http.ResponseWriter, r *http.Request) {
    vars := mux.Vars(r)
    cartID, _ := uuid.Parse(vars["cartID"])
    
    var body struct {
        ProductID string `json:"product_id"`
        Quantity  int    `json:"quantity"`
    }
    json.NewDecoder(r.Body).Decode(&body)
    
    productID, _ := uuid.Parse(body.ProductID)
    
    req := Request{
        CartID:    cartID,
        ProductID: productID,
        Quantity:  body.Quantity,
    }
    
    resp, err := h.endpoint(r.Context(), req)
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(resp)
}

func (h *HTTPHandler) RegisterRoutes(router *mux.Router) {
    router.HandleFunc("/storefront/carts/{cartID}/items", h.Handle).Methods("POST")
}
```

**When to use Screens**:
- Different actors need different behavior (storefront vs backoffice)
- Need to orchestrate multiple slices
- Need actor-specific validation or authorization
- Different response formats for different clients

### 4. Processors (Background Work)

Processors handle background/asynchronous work triggered by events.

```go
// processors/archive_items/processor.go
package archiveitems

import (
    "context"
    "log"
    "time"
    "github.com/terraskye/eventsourcing"
    "yourapp/events"
    "yourapp/slices/archiveitem"
)

type Processor struct {
    archiveHandler eventsourcing.CommandHandler[archiveitem.ArchiveItem]
}

func NewProcessor(archiveHandler eventsourcing.CommandHandler[archiveitem.ArchiveItem]) *Processor {
    return &Processor{archiveHandler: archiveHandler}
}

// OnCartSubmitted archives items when cart is submitted
func (p *Processor) OnCartSubmitted(ctx context.Context, event *events.CartSubmitted) error {
    // Wait 30 days before archiving
    time.Sleep(30 * 24 * time.Hour) // In production, use a job queue
    
    for _, itemID := range event.ItemIDs {
        cmd := archiveitem.ArchiveItem{
            CartID: event.CartID,
            ItemID: itemID,
        }
        
        if _, err := p.archiveHandler(ctx, cmd); err != nil {
            log.Printf("Failed to archive item %s: %v", itemID, err)
        }
    }
    
    return nil
}

func (p *Processor) Subscribe(eventBus eventsourcing.EventBus) error {
    handlers := []eventsourcing.EventHandler{
        eventsourcing.OnEvent(p.OnCartSubmitted),
    }
    
    group := eventsourcing.NewEventGroupProcessor(handlers...)
    return eventBus.Subscribe(context.Background(), "archive-items-processor", group)
}
```

## Wiring It All Together

```go
// cmd/api/main.go
package main

import (
    "log"
    "net/http"
    
    "github.com/gorilla/mux"
    "github.com/terraskye/eventsourcing"
    "github.com/terraskye/eventsourcing/eventstore/memory"
    
    "yourapp/slices/additem"
    "yourapp/slices/removeitem"
    "yourapp/slices/cartitems"
    "yourapp/slices/inventories"
    "yourapp/screens/storefrontadditem"
    "yourapp/screens/backofficeadditem"
    "yourapp/processors/archiveitems"
)

func main() {
    // Infrastructure
    store := memory.NewEventStore()
    eventBus := eventsourcing.NewEventBus()
    queryBus := eventsourcing.NewQueryBus()
    
    // Command handlers
    addItemHandler := additem.NewHandler(store)
    removeItemHandler := removeitem.NewHandler(store)
    archiveItemHandler := archiveitem.NewHandler(store)
    
    // Query handlers
    cartItemsQuery := cartitems.NewQueryHandler(store)
    inventoryQuery := inventories.NewQueryHandler(store)
    
    // Register queries
    eventsourcing.RegisterQueryHandler(queryBus, cartItemsQuery)
    eventsourcing.RegisterQueryHandler(queryBus, inventoryQuery)
    
    // Screens
    storefrontService := storefrontadditem.NewService(addItemHandler, inventoryQuery)
    storefrontEndpoint := storefrontadditem.MakeEndpoint(storefrontService)
    storefrontHTTP := storefrontadditem.NewHTTPHandler(storefrontEndpoint)
    
    backofficeService := backofficeadditem.NewService(addItemHandler)
    backofficeEndpoint := backofficeadditem.MakeEndpoint(backofficeService)
    backofficeHTTP := backofficeadditem.NewHTTPHandler(backofficeEndpoint)
    
    // Processors
    archiveProcessor := archiveitems.NewProcessor(archiveItemHandler)
    archiveProcessor.Subscribe(eventBus)
    
    // HTTP Router
    router := mux.NewRouter()
    
    // Register slice routes
    additem.NewHTTPHandler(addItemHandler).RegisterRoutes(router)
    removeitem.NewHTTPHandler(removeItemHandler).RegisterRoutes(router)
    cartitems.NewHTTPHandler(cartItemsQuery).RegisterRoutes(router)
    
    // Register screen routes
    storefrontHTTP.RegisterRoutes(router)
    backofficeHTTP.RegisterRoutes(router)
    
    // Start server
    log.Println("Server starting on :8080")
    log.Fatal(http.ListenAndServe(":8080", router))
}
```

## Best Practices

### 1. Keep Slices Independent

✅ **DO**: Each slice is self-contained
```go
// slices/add_item/command.go contains everything
// - Command
// - State
// - Evolve
// - Decide
// - Handler factory
```

❌ **DON'T**: Share business logic between slices
```go
// shared/cart_validator.go
// This creates coupling!
```

### 2. Events are the Contract

✅ **DO**: Share events between slices
```go
// slices/add_item emits events.ItemAdded
// slices/cartview consumes events.ItemAdded
// Connected via events, not code
```

### 3. Screens Compose Slices

✅ **DO**: Use screens for orchestration
```go
// screens/storefront combines:
// - additem slice
// - inventories slice
// - pricing slice
```

❌ **DON'T**: Put orchestration in slices
```go
// slices/add_item/command.go
// Don't check inventory here!
```

### 4. Minimize Shared Code

Only share:
- ✅ Events (domain facts)
- ✅ Value objects (Money, Email, etc.)
- ✅ Infrastructure (middleware, config)

Don't share:
- ❌ Business logic
- ❌ Validation
- ❌ State

### 5. One Slice = One Thing

✅ **DO**: Single responsibility
```
slices/
  ├── add_item/        # Does ONE thing
  ├── remove_item/     # Does ONE thing
  └── change_quantity/ # Does ONE thing
```

❌ **DON'T**: Multi-purpose slices
```
slices/
  └── cart_management/  # Does too many things!
```

## Testing Slices

Each slice is independently testable:

```go
// slices/add_item/command_test.go
package additem_test

import (
    "context"
    "testing"
    "github.com/terraskye/eventsourcing/eventstore/memory"
    "yourapp/slices/additem"
)

func TestAddItem_Success(t *testing.T) {
    // Setup
    store := memory.NewEventStore()
    handler := additem.NewHandler(store)
    
    // Execute
    cmd := additem.AddItem{
        CartID:    uuid.New(),
        ProductID: uuid.New(),
        Quantity:  2,
        Price:     1999,
    }
    
    result, err := handler(context.Background(), cmd)
    
    // Assert
    if err != nil {
        t.Fatal(err)
    }
    if !result.Successful {
        t.Error("expected success")
    }
}
```

## Summary

Vertical Slice Architecture with terraskye/eventsourcing:

1. **Events**: Shared domain facts
2. **Slices**: Self-contained features (commands or queries)
3. **Screens**: Actor-specific compositions
4. **Processors**: Background work

**Benefits**:
- ✅ Easy to understand (one slice = one feature)
- ✅ Easy to change (changes localized)
- ✅ Easy to test (slices are independent)
- ✅ Easy to scale teams (own slices)
- ✅ Natural fit for event sourcing

**Next**: See the [Shopping Cart Example](../examples/shopping-cart/) for a complete implementation.