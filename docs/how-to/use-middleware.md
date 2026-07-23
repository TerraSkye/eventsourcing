# How to use middleware

Middleware adds cross-cutting behaviour — logging, telemetry, authentication — once,
without wrapping each handler individually at registration.

## CommandBus

Call `Use()` to register middleware on the bus. The first argument is the outermost wrapper and executes first on each dispatch.

**Call `Use()` before `Register()`.** The middleware chain is baked into each handler at registration time, from the middlewares present at that moment. Handlers already registered are *not* re-wrapped, so middleware added later never runs for them. Treat `Use()` as startup wiring:

```go
bus := eventsourcing.NewCommandBus(100, 4)

bus.Use(
    logging.CommandLogging(logger),
    otel.CommandTelemetry(),
)

eventsourcing.Register(bus, placeOrderHandler)
eventsourcing.Register(bus, confirmOrderHandler)
```

Calling `Use()` multiple times appends to the chain, but only affects handlers registered *after* each call:

```go
bus.Use(logging.CommandLogging(logger))
eventsourcing.Register(bus, placeOrderHandler)  // logging only

bus.Use(otel.CommandTelemetry())
eventsourcing.Register(bus, confirmOrderHandler) // logging + telemetry
```

Here `placeOrderHandler` never gets telemetry. Register every middleware first to avoid this.

### Writing a function middleware

```go
var rateLimiter eventsourcing.CommandHandlerMiddleware = func(
    next eventsourcing.CommandHandler[eventsourcing.Command],
) eventsourcing.CommandHandler[eventsourcing.Command] {
    return func(ctx context.Context, cmd eventsourcing.Command) (eventsourcing.AppendResult, error) {
        if !myLimiter.Allow() {
            return eventsourcing.AppendResult{}, errors.New("rate limit exceeded")
        }
        return next(ctx, cmd)
    }
}

bus.Use(rateLimiter)
```

`next` is a `CommandHandler[Command]`, not a bare `func(...)`. Because `CommandHandler` is a named type, spelling the parameter out as a raw function signature will not compile.

### Writing a struct middleware

Implement a `Middleware()` method with the same signature as `CommandHandlerMiddleware`. This is useful when the middleware carries state or configuration.

```go
type AuditMiddleware struct {
    logger *slog.Logger
    topic  string
}

func (a *AuditMiddleware) Middleware(
    next eventsourcing.CommandHandler[eventsourcing.Command],
) eventsourcing.CommandHandler[eventsourcing.Command] {
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

Call `Use()` to register middleware on the bus. As with the `CommandBus`, the chain is baked in at registration time — **call `Use()` before `RegisterQueryHandler()`**, since handlers already registered are not re-wrapped. Middleware receives the query as a `Query`; use `fmt.Sprintf("%T", qry)` to get the concrete type name.

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
var myMiddleware eventsourcing.QueryHandlerMiddleware = func(
    next eventsourcing.QueryGateway[eventsourcing.Query, any],
) eventsourcing.QueryGateway[eventsourcing.Query, any] {
    return func(ctx context.Context, qry eventsourcing.Query) (any, error) {
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

`EventBus` is an interface, and each implementation carries its own `Use()` method. Call it before subscribing: middleware is applied to a handler when it is passed to `Subscribe`, so subscriptions made earlier are not re-wrapped.

```go
bus := memory.NewEventBus(100)

bus.Use(
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

`EventStore` is an interface. An `EventStoreMiddleware` is applied by wrapping the store directly — call it and reassign:

```go
store = otel.EventStoreTelemetry()(store)
```

To compose several, wrap outward from the innermost:

```go
store = otel.EventStoreTelemetry()(store)
store = myAuditMiddleware(store)  // outermost, runs first
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
| `logging.CommandLogging(logger)` | `CommandHandlerMiddleware` | `bus.Use(...)` before `Register` |
| `otel.CommandTelemetry(...)` | `CommandHandlerMiddleware` | `bus.Use(...)` before `Register` |
| `logging.QueryLogging(logger)` | `QueryHandlerMiddleware` | `bus.Use(...)` before `RegisterQueryHandler` |
| `otel.QueryTelemetry(...)` | `QueryHandlerMiddleware` | `bus.Use(...)` before `RegisterQueryHandler` |
| `logging.EventLogging(logger)` | `EventHandlerMiddleware` | `bus.Use(...)` before `Subscribe` |
| `otel.EventBusTelemetry(...)` | `EventHandlerMiddleware` | `bus.Use(...)` before `Subscribe` |
| `otel.EventStoreTelemetry(...)` | `EventStoreMiddleware` | `store = mw(store)` |
