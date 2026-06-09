package eventsourcing

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// queryBusMiddlewareHandler is the interface for internal storage of query
// bus middlewares. Both QueryBusMiddleware and struct-based middlewares
// satisfy it.
type queryBusMiddlewareHandler interface {
	Middleware(
		next func(ctx context.Context, qry any) (any, error),
	) func(ctx context.Context, qry any) (any, error)
}

// QueryBusMiddleware wraps a type-erased query handler, allowing pre/post
// processing of queries across all registered handlers.
type QueryBusMiddleware func(
	next func(ctx context.Context, qry any) (any, error),
) func(ctx context.Context, qry any) (any, error)

// Middleware implements queryBusMiddlewareHandler.
func (mw QueryBusMiddleware) Middleware(
	next func(ctx context.Context, qry any) (any, error),
) func(ctx context.Context, qry any) (any, error) {
	return mw(next)
}

// QueryBus acts as a central registry for query handlers. It stores
// handlers keyed by their query and result types, allowing multiple
// query types to be registered in a single bus.
//
// Handlers can later be executed via a typed GenericQueryGateway.
//
// Example Usage:
//
//	bus := NewQueryBus()
//	RegisterQueryHandlerFunc(bus, store.GetTask)
//	RegisterQueryHandlerFunc(bus, store.ListTasks)
type QueryBus struct {
	mu          sync.RWMutex
	handlers    map[string]any
	rawHandlers map[string]any
	builders    map[string]func()
	requestees  map[string]struct{}
	middlewares []queryBusMiddlewareHandler
}

// NewQueryBus creates a new, empty QueryBus ready for handler registration.
func NewQueryBus() *QueryBus {
	return &QueryBus{
		handlers:    make(map[string]any),
		rawHandlers: make(map[string]any),
		builders:    make(map[string]func()),
		requestees:  make(map[string]struct{}),
	}
}

// Use adds one or more middlewares to the bus. All handlers — registered
// before or after the call — are re-wrapped with the full middleware chain.
// The first argument is the outermost wrapper and executes first on each query.
func (q *QueryBus) Use(middlewares ...QueryBusMiddleware) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, mw := range middlewares {
		q.middlewares = append(q.middlewares, mw)
	}
	q.rebuildHandlers()
}

// useMiddleware adds a struct-based middleware to the chain.
func (q *QueryBus) useMiddleware(mw queryBusMiddlewareHandler) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.middlewares = append(q.middlewares, mw)
	q.rebuildHandlers()
}

func (q *QueryBus) rebuildHandlers() {
	for _, build := range q.builders {
		build()
	}
}

// wrapQueryHandler applies the middleware chain around a typed QueryHandler,
// using type erasure internally so a single middleware type covers all queries.
func wrapQueryHandler[T Query, R any](h QueryHandler[T, R], middlewares []queryBusMiddlewareHandler) QueryHandler[T, R] {
	if len(middlewares) == 0 {
		return h
	}
	anyNext := func(ctx context.Context, qry any) (any, error) {
		return h.HandleQuery(ctx, qry.(T))
	}
	for i := len(middlewares) - 1; i >= 0; i-- {
		anyNext = middlewares[i].Middleware(anyNext)
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

// HandlerOption represents an optional configuration function that can
// modify handler behavior or metadata. Currently reserved for future
// extensions such as worker pools, timeouts, or rate limiting.
type HandlerOption func(*handlerSettings)

// handlerSettings stores internal configuration for a registered handler.
// TODO expand with querybus settings
type handlerSettings struct {
}

// RegisterQueryHandlerFunc registers a plain function as a query handler.
// Type parameters are inferred from the function signature. Prefer this over
// RegisterQueryHandler when registering method values from a provider struct.
//
// Panics if a handler for the same query and result types is already registered.
//
// Example Usage:
//
//	RegisterQueryHandlerFunc(bus, store.GetTask)
//	RegisterQueryHandlerFunc(bus, store.ListTasks)
func RegisterQueryHandlerFunc[T Query, R any](bus *QueryBus, fn queryHandlerFunc[T, R], opts ...HandlerOption) {
	RegisterQueryHandler(bus, fn, opts...)
}

// RegisterQueryHandler registers a QueryHandler[T, R] on the bus. Use this
// when registering a type that explicitly implements the QueryHandler interface.
// For plain functions or method values, prefer RegisterQueryHandlerFunc.
//
// Panics if a handler for the same query and result types is already registered.
//
// Example Usage:
//
//	RegisterQueryHandler(bus, myHandler)
func RegisterQueryHandler[T Query, R any](bus *QueryBus, handler QueryHandler[T, R], opts ...HandlerOption) {
	key := fmt.Sprintf("%T|%T", *new(T), *new(R))

	bus.mu.Lock()
	defer bus.mu.Unlock()

	if _, exists := bus.rawHandlers[key]; exists {
		panic(ErrDuplicateHandler)
	}

	bus.rawHandlers[key] = handler
	bus.builders[key] = func() {
		bus.handlers[key] = wrapQueryHandler[T, R](handler, bus.middlewares)
	}
	bus.builders[key]()

	meta := &handlerSettings{}
	for _, opt := range opts {
		opt(meta)
	}
	//bus.settings[key] = meta
}

func (q QueryBus) Validate() error {
	errs := make([]error, 0)
	for requestee := range q.requestees {
		if _, ok := q.handlers[requestee]; !ok {
			errs = append(errs, fmt.Errorf("unknown query handler: %s", requestee))
		}
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}
