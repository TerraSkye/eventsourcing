package eventsourcing

import (
	"errors"
	"fmt"
	"sync"
)

// QueryBus is a central registry of query handlers, keyed by their query
// and result types, so that multiple query types can be registered on a
// single bus. Handlers are executed through a typed [QueryGateway] created
// with [NewQueryGateway].
//
// Example Usage:
//
//	bus := NewQueryBus()
//	RegisterQueryHandlerFunc(bus, store.GetTask)
//	RegisterQueryHandlerFunc(bus, store.ListTasks)
type QueryBus struct {
	mu          sync.RWMutex
	handlers    map[string]any
	requestees  map[string]struct{}
	middlewares []QueryHandlerMiddleware
}

// NewQueryBus creates a new, empty QueryBus ready for handler registration.
func NewQueryBus() *QueryBus {
	return &QueryBus{
		handlers:   make(map[string]any),
		requestees: make(map[string]struct{}),
	}
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

	if _, exists := bus.handlers[key]; exists {
		panic(ErrDuplicateHandler)
	}

	bus.handlers[key] = wrapQueryHandler[T, R](handler, bus.middlewares)

	meta := &handlerSettings{}
	for _, opt := range opts {
		opt(meta)
	}
}

// Validate reports an error listing every query/result type pair that a
// [QueryGateway] was created for via [NewQueryGateway] but that has no
// registered handler. Call it during startup, after all gateways and
// handlers are wired up, to catch a missing registration before it can
// surface as a runtime error.
func (q *QueryBus) Validate() error {
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
