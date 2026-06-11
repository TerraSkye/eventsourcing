package eventsourcing

import "context"

// CommandHandlerMiddleware defines a function type for decorating command handlers on the CommandBus.
// Registering one or more middlewares via Use() causes every command handler — regardless of
// when it was registered — to be wrapped with the full middleware chain.
//
// Parameters:
//   - next: The next handler in the chain. Call it to pass the command to the following
//     middleware or the final registered handler.
//
// Returns:
//   - A wrapped handler that intercepts command execution.
//
// Notes:
//   - The first middleware passed to Use() is the outermost wrapper and executes first on each dispatch.
//
// Example Usage:
//
//	var rateLimiter eventsourcing.CommandHandlerMiddleware = func(
//	    next eventsourcing.CommandHandler[eventsourcing.Command],
//	) eventsourcing.CommandHandler[eventsourcing.Command] {
//	    return func(ctx context.Context, cmd eventsourcing.Command) (eventsourcing.AppendResult, error) {
//	        if !myLimiter.Allow() {
//	            return eventsourcing.AppendResult{}, errors.New("rate limit exceeded")
//	        }
//	        return next(ctx, cmd)
//	    }
//	}
//	bus.Use(rateLimiter)
type CommandHandlerMiddleware func(next CommandHandler[Command]) CommandHandler[Command]

// Use adds one or more middlewares to the CommandBus and immediately re-wraps every
// registered command handler with the updated chain.
//
// Parameters:
//   - middlewares: One or more CommandHandlerMiddleware values to append to the chain.
//
// Notes:
//   - The first middleware in the list is the outermost wrapper and executes first on each dispatch.
//   - Must be called before Register; middleware is baked into the handler at registration time.
//
// Example Usage:
//
//	bus.Use(
//	    logging.CommandLogging(logger),
//	    otel.CommandTelemetry(),
//	)
func (b *CommandBus) Use(middlewares ...CommandHandlerMiddleware) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, mw := range middlewares {
		b.middlewares = append(b.middlewares, mw)
	}
}

// QueryHandlerMiddleware defines a function type for decorating query handlers on the QueryBus.
// Registering one or more middlewares via Use() causes every query handler — regardless of
// when it was registered — to be wrapped with the full middleware chain.
//
// The query is received as a Query interface, so qry.ID() is available directly.
// Use fmt.Sprintf("%T", qry) to retrieve the concrete query type name.
// The result is received as any and holds the concrete result value.
//
// Parameters:
//   - next: The next handler in the chain. Call it to pass the query to the following
//     middleware or the final registered handler.
//
// Returns:
//   - A wrapped handler that intercepts query execution.
//
// Notes:
//   - The first middleware passed to Use() is the outermost wrapper and executes first on each query.
//
// Example Usage:
//
//	var myMiddleware eventsourcing.QueryHandlerMiddleware = func(
//	    next eventsourcing.QueryGateway[eventsourcing.Query, any],
//	) eventsourcing.QueryGateway[eventsourcing.Query, any] {
//	    return func(ctx context.Context, qry eventsourcing.Query) (any, error) {
//	        // before
//	        result, err := next(ctx, qry)
//	        // after
//	        return result, err
//	    }
//	}
//	bus.Use(myMiddleware)
type QueryHandlerMiddleware func(next QueryGateway[Query, any]) QueryGateway[Query, any]

// Use adds one or more middlewares to the QueryBus.
// Must be called before RegisterQueryHandler; middleware is baked in at registration time.
//
// Example Usage:
//
//	bus.Use(
//	    logging.QueryLogging(logger),
//	    otel.QueryTelemetry(),
//	)
func (q *QueryBus) Use(middlewares ...QueryHandlerMiddleware) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, mw := range middlewares {
		q.middlewares = append(q.middlewares, mw)
	}
}

// wrapQueryHandler applies the middleware chain to a typed QueryHandler[T, R].
// It bridges the generic handler signature to the middleware's any-typed signature,
// restoring the concrete result type before returning to the caller.
func wrapQueryHandler[T Query, R any](h QueryHandler[T, R], middlewares []QueryHandlerMiddleware) QueryHandler[T, R] {
	if len(middlewares) == 0 {
		return h
	}
	anyNext := QueryGateway[Query, any](func(ctx context.Context, qry Query) (any, error) {
		return h.HandleQuery(ctx, qry.(T))
	})
	for i := len(middlewares) - 1; i >= 0; i-- {
		anyNext = middlewares[i](anyNext)
	}
	return queryHandlerFunc[T, R](func(ctx context.Context, qry T) (R, error) {
		result, err := anyNext(ctx, qry)
		if err != nil {
			var zero R
			return zero, err
		}
		if result == nil {
			var zero R
			return zero, nil
		}
		return result.(R), nil
	})
}

// EventHandlerMiddleware defines a function type for decorating event handlers on an EventBus.
// Middlewares are applied when a handler is passed to Subscribe, wrapping it with
// cross-cutting concerns such as logging, telemetry, or error handling.
//
// Parameters:
//   - next: The next EventHandler in the chain. Call it to pass the event to the
//     following middleware or the final registered handler.
//
// Returns:
//   - A wrapped EventHandler that intercepts event handling.
//
// Notes:
//   - The first middleware passed to Use() is the outermost wrapper and executes first for each event.
//
// Example Usage:
//
//	var myMiddleware eventsourcing.EventHandlerMiddleware = func(next eventsourcing.EventHandler) eventsourcing.EventHandler {
//	    return eventsourcing.NewEventHandlerFunc(func(ctx context.Context, event eventsourcing.Event) error {
//	        // before
//	        err := next.Handle(ctx, event)
//	        // after
//	        return err
//	    })
//	}
type EventHandlerMiddleware func(next EventHandler) EventHandler

// EventStoreMiddleware defines a function type for decorating an EventStore.
// Apply one by wrapping a store directly: store = mw(store).
//
// Parameters:
//   - next: The next EventStore in the chain. Delegate calls to it to preserve the
//     original store's behaviour.
//
// Returns:
//   - A decorated EventStore that intercepts one or more store operations.
//
// Notes:
//   - A common implementation embeds EventStore and overrides only the methods of interest.
//
// Example Usage:
//
//	var metered eventsourcing.EventStoreMiddleware = func(next eventsourcing.EventStore) eventsourcing.EventStore {
//	    return &meteredStore{next: next, counter: myCounter}
//	}
//	store = metered(baseStore)
type EventStoreMiddleware func(next EventStore) EventStore
