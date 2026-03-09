# Vertical slice architecture

## What is it?

Vertical slice architecture organises code by **feature** rather than by **technical layer**. Each slice contains everything needed for a single use case — from HTTP handling to business logic to data access.

## Why not layers?

Traditional layered architecture groups files by their technical role:

```
controllers/
  cart_controller.go
  order_controller.go
services/
  cart_service.go
  order_service.go
repositories/
  cart_repository.go
  order_repository.go
```

A change to "add item to cart" touches all four layers across multiple directories. The layers create coupling: shared abstractions in `services/` or `repositories/` become sources of accidental complexity.

## Vertical slices

Each feature lives in one directory:

```
slices/
  add_item/
    command.go    ← Command + handler + business logic
    http.go       ← HTTP endpoint
  remove_item/
    command.go
    http.go
  cart_items/     ← Query slice
    query.go
    readmodel.go
    http.go
```

A change to "add item to cart" only touches `slices/add_item/`.

## Recommended structure

```
your-app/
├── cmd/
│   └── api/
│       └── main.go              # Wiring only — no business logic
│
├── events/                      # Shared domain events
│   ├── item_added.go
│   └── cart_submitted.go
│
├── slices/                      # Feature slices
│   ├── add_item/                # Command slice
│   │   ├── command.go           # Command, state, evolve, decide, NewHandler
│   │   └── http.go              # HTTP handler
│   │
│   ├── cart_items/              # Query slice (live read model)
│   │   ├── query.go
│   │   ├── readmodel.go
│   │   └── http.go
│   │
│   └── cart_totals/             # Query slice (cached projection)
│       ├── query.go
│       ├── readmodel.go
│       ├── projector.go         # Event handlers + cache
│       └── http.go
│
├── screens/                     # Actor-specific compositions
│   └── storefront_add_item/
│       ├── service.go           # Orchestrates multiple slices
│       ├── endpoint.go          # Request/response mapping
│       └── http.go
│
├── processors/                  # Background processors
│   └── archive_items/
│       └── processor.go         # Reacts to events, issues commands
│
└── shared/                      # Truly shared code only
    └── domain/
        └── money.go             # Value objects
```

## The four building blocks

### Slices (features)

A slice is a self-contained feature. There are two kinds:

**Command slice**: changes state.
- Contains: `Command` struct, `taskState` struct, `evolve`, `decide`, `NewHandler`.
- One command type per slice — do not merge unrelated commands.

**Query slice**: reads state.
- Contains: `Query` struct, read model struct, `QueryHandler`, `evolve` (for the read model).
- Choose between live (rebuild on every query) or cached (rebuild once, update via event bus).

### Events (the shared contract)

Events are the **only** shared code between slices. They represent facts and carry the data needed by any consumer.

Events must never contain business logic. Slices communicate through events, not through direct function calls.

### Screens (compositions)

A screen combines multiple slices for a specific actor or use case. For example, "storefront add item" checks inventory (via a query slice) before delegating to "add item to cart" (via a command slice).

Use screens when:
- Different actors (storefront vs. backoffice) need different behaviour for the same underlying operation.
- An operation requires orchestrating multiple slices.
- You need actor-specific validation or response format.

### Processors (background work)

A processor reacts to events and issues commands. It bridges slices asynchronously.

Example: when `OrderShipped` is received, schedule a follow-up email 3 days later by issuing a `SendFollowUpEmail` command.

## Rules

1. **Slices do not import other slices.** If two slices need to communicate, they do so through events and the event bus.
2. **Events are the only shared domain code.** Value objects (Money, Email) may also be shared, but sparingly.
3. **Screens import slices, slices do not import screens.**
4. **`main.go` handles wiring only** — no business logic, no HTTP routing logic, just creating handlers and connecting them.

## Benefits

- A new developer can understand a feature by reading one directory.
- Changes are localised — touching one slice cannot break another.
- Slices can be extracted into separate services with minimal refactoring.
- Teams can own slices independently.

## See also

- [Tutorial: Building a task management API](../tutorials/index.md) for a worked example of this structure.
