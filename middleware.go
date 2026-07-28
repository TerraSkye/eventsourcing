package eventsourcing

import "context"

// CommandHandlerMiddleware decorates a command handler on a [CommandBus] with a
// cross-cutting concern such as logging, telemetry, or rate limiting. It receives
// the next handler in the chain and returns a handler that wraps it; call next to
// pass the command along, or return without calling it to short-circuit.
//
// The chain is baked into a handler once, by [Register], from the middlewares
// added via [CommandBus.Use] up to that moment. Add all middleware during startup
// wiring, before registering any handler. The first middleware passed to Use is
// the outermost wrapper and runs first on each dispatch.
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

// Use appends middlewares to the chain applied to command handlers. The first
// one appended is the outermost wrapper and runs first on each dispatch.
//
// Use must be called before [Register]: the chain is baked into each handler at
// registration time, so handlers already registered are not re-wrapped and a
// later Use call has no effect on them. Configure the full chain during startup
// wiring.
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

// QueryHandlerMiddleware decorates a query handler on a [QueryBus]. It receives
// the next handler in the chain and returns a handler that wraps it; call next to
// pass the query along, or return without calling it to short-circuit.
//
// The query arrives as a [Query], so qry.ID is available directly; use
// fmt.Sprintf("%T", qry) for the concrete type name. The result arrives as an any
// holding the concrete result value.
//
// The chain is baked into a handler once, by [RegisterQueryHandler], from the
// middlewares added via [QueryBus.Use] up to that moment. Add all middleware
// during startup wiring, before registering any handler. The first middleware
// passed to Use is the outermost wrapper and runs first on each query.
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

// Use appends middlewares to the chain applied to query handlers. The first
// one appended is the outermost wrapper and runs first on each query.
//
// Use must be called before [RegisterQueryHandler] or
// [RegisterQueryHandlerFunc]: the chain is baked into each handler at
// registration time, so handlers already registered are not re-wrapped and a
// later Use call has no effect on them. Configure the full chain during
// startup wiring.
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

// EventHandlerMiddleware decorates an event handler on an [EventBus] with a
// cross-cutting concern such as logging, telemetry, or error handling. It
// receives the next handler in the chain and returns a handler that wraps
// it; call next to pass the event along, or return without calling it to
// short-circuit. Middleware is applied when a handler is passed to
// [EventBus.Subscribe], and the first middleware passed to
// [EventBus.Use] is the outermost wrapper and runs first for each event.
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

// EventStoreMiddleware decorates an [EventStore], typically to intercept one
// or more of its methods while delegating the rest to next. Apply one by
// wrapping a store directly: store = mw(store). A common implementation
// embeds EventStore for the pass-through methods and overrides only the
// ones of interest.
//
// Example Usage:
//
//	var metered eventsourcing.EventStoreMiddleware = func(next eventsourcing.EventStore) eventsourcing.EventStore {
//	    return &meteredStore{next: next, counter: myCounter}
//	}
//	store = metered(baseStore)
type EventStoreMiddleware func(next EventStore) EventStore
