package eventsourcing

import (
	"errors"
	"fmt"
	"sync"
)

// QueryBus is a central registry for query handlers, keyed by query and result
// type. Handlers are executed via a typed QueryGateway.
//
// Example Usage:
//
//	bus := NewQueryBus()
//	RegisterQueryHandlerFunc(bus, store.GetTask)
//	RegisterQueryHandlerFunc(bus, store.ListTasks)
type QueryBus struct {
	mu         sync.RWMutex
	handlers   map[string]any
	requestees map[string]struct{}
}

// NewQueryBus creates a new, empty QueryBus ready for handler registration.
func NewQueryBus() *QueryBus {
	return &QueryBus{
		mu:         sync.RWMutex{},
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

	bus.handlers[key] = handler

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
