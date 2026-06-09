# How to use middleware

Middleware adds cross-cutting behaviour — logging, telemetry, authentication — once,
without wrapping each handler individually at registration.

## CommandBus

Call `Use()` to register middleware on the bus. All handlers — registered before or after the call — are wrapped with the full middleware chain. The first argument is the outermost wrapper and executes first on each dispatch.

```go
bus := eventsourcing.NewCommandBus(100, 4)

bus.Use(
    logging.CommandLogging(logger),
    otel.CommandTelemetry(),
)

eventsourcing.Register(bus, placeOrderHandler)
eventsourcing.Register(bus, confirmOrderHandler)
```

Calling `Use()` multiple times appends to the chain:

```go
bus.Use(logging.CommandLogging(logger))
eventsourcing.Register(bus, placeOrderHandler)

bus.Use(otel.CommandTelemetry()) // re-wraps placeOrderHandler and all future handlers
eventsourcing.Register(bus, confirmOrderHandler)
```

### Writing a function middleware

```go
var rateLimiter eventsourcing.CommandBusMiddleware = func(
    next func(ctx context.Context, cmd eventsourcing.Command) (eventsourcing.AppendResult, error),
) func(ctx context.Context, cmd eventsourcing.Command) (eventsourcing.AppendResult, error) {
    return func(ctx context.Context, cmd eventsourcing.Command) (eventsourcing.AppendResult, error) {
        if !myLimiter.Allow() {
            return eventsourcing.AppendResult{}, errors.New("rate limit exceeded")
        }
        return next(ctx, cmd)
    }
}

bus.Use(rateLimiter)
```

### Writing a struct middleware

Implement a `Middleware()` method with the same signature as `CommandBusMiddleware`. This is useful when the middleware carries state or configuration.

```go
type AuditMiddleware struct {
    logger *slog.Logger
    topic  string
}

func (a *AuditMiddleware) Middleware(
    next func(ctx context.Context, cmd eventsourcing.Command) (eventsourcing.AppendResult, error),
) func(ctx context.Context, cmd eventsourcing.Command) (eventsourcing.AppendResult, error) {
    return func(ctx context.Context, cmd eventsourcing.Command) (eventsourcing.AppendResult, error) {
        a.logger.Info("audit", "topic", a.topic, "command", fmt.Sprintf("%T", cmd))
        return next(ctx, cmd)
    }
}
```

Pass it to `bus.Use()` via its `Middleware` method:

```go
audit := &AuditMiddleware{logger: logger, topic: "orders"}
bus.Use(audit.Middleware)
```

---

## QueryBus

Call `Use()` to register middleware on the bus. All handlers — registered before or after the call — are wrapped with the full middleware chain. Middleware receives the query as `any`; use `fmt.Sprintf("%T", qry)` to get the type name.

```go
bus := eventsourcing.NewQueryBus()

bus.Use(
    logging.QueryLogging(logger),
    otel.QueryTelemetry(),
)

eventsourcing.RegisterQueryHandler[GetOrder, *Order](bus, getOrderHandler)
eventsourcing.RegisterQueryHandler[ListOrders, *OrderList](bus, listOrdersHandler)
```

### Writing a query bus middleware

```go
var myMiddleware eventsourcing.QueryBusMiddleware = func(
    next func(ctx context.Context, qry any) (any, error),
) func(ctx context.Context, qry any) (any, error) {
    return func(ctx context.Context, qry any) (any, error) {
        // before
        result, err := next(ctx, qry)
        // after
        return result, err
    }
}

bus.Use(myMiddleware)
```

---

## EventBus

`EventBus` is an interface. Wrap it with `NewEventBusWithMiddleware` so that every handler passed to `Subscribe` is automatically wrapped.

```go
bus = eventsourcing.NewEventBusWithMiddleware(bus,
    logging.EventLogging(logger),
    otel.EventBusTelemetry(),
)

bus.Subscribe(ctx, "order-projector", orderProjector)
bus.Subscribe(ctx, "invoice-projector", invoiceProjector)
```

### Writing an event handler middleware

```go
var myMiddleware eventsourcing.EventHandlerMiddleware = func(next eventsourcing.EventHandler) eventsourcing.EventHandler {
    return eventsourcing.NewEventHandlerFunc(func(ctx context.Context, event eventsourcing.Event) error {
        // before
        err := next.Handle(ctx, event)
        // after
        return err
    })
}
```

---

## EventStore

`EventStore` is an interface. Use `ApplyEventStoreMiddleware` to compose a chain of `EventStoreMiddleware` values around a store.

```go
store = eventsourcing.ApplyEventStoreMiddleware(store,
    otel.EventStoreTelemetry(),
)
```

### Writing an event store middleware

```go
var metered eventsourcing.EventStoreMiddleware = func(next eventsourcing.EventStore) eventsourcing.EventStore {
    return &meteredStore{next: next, counter: myCounter}
}
```

---

## Available middleware factories

| Factory | Type returned | Apply with |
|---|---|---|
| `logging.CommandLogging(logger)` | `CommandBusMiddleware` | `bus.Use(...)` |
| `otel.CommandTelemetry(...)` | `CommandBusMiddleware` | `bus.Use(...)` |
| `logging.QueryLogging(logger)` | `QueryBusMiddleware` | `bus.Use(...)` |
| `otel.QueryTelemetry(...)` | `QueryBusMiddleware` | `bus.Use(...)` |
| `logging.EventLogging(logger)` | `EventHandlerMiddleware` | `NewEventBusWithMiddleware(...)` |
| `otel.EventBusTelemetry(...)` | `EventHandlerMiddleware` | `NewEventBusWithMiddleware(...)` |
| `otel.EventStoreTelemetry(...)` | `EventStoreMiddleware` | `ApplyEventStoreMiddleware(...)` |
