package eventsourcing

import (
	"fmt"

	"github.com/io-da/query"
)

// QueryBus acts as a central registry for query handlers. It stores
// handlers keyed by their query and result types, allowing multiple
// query types to be registered in a single bus.
//
// Handlers can later be executed via a typed GenericQueryGateway.
//
// Example Usage:
//
//	bus := NewQueryBus()
//	RegisterQueryHandler[MyQuery, *MyResult](bus, NewQueryHandlerFunc(func(ctx context.Context, q MyQuery) (*MyResult, error) {
//	    return &MyResult{Value: 42}, nil
//	}))
type QueryBus struct {
	handlers map[string]any
}

// NewQueryBus creates a new QueryBus instance.
//
// Returns:
//   - *QueryBus: A new, empty bus ready for handler registration.
func NewQueryBus() *QueryBus {
	return &QueryBus{
		handlers: make(map[string]any),
	}
}

// HandlerOption represents an optional configuration function that can
// modify handler behavior or metadata. Currently reserved for future
// extensions such as worker pools, timeouts, or rate limiting.
type HandlerOption func(*handlerSettings)

// handlerSettings stores internal configuration for a registered handler.
type handlerSettings struct {
}

// RegisterQueryHandler registers a QueryHandler for a specific query
// and result type on the provided QueryBus.
//
// This function generates a unique key from the types of T and R,
// stores the handler in the bus, and applies any optional configuration.
//
// Type Parameters:
//   - T: The query type implementing query.Query.
//   - R: The result type (ReadModel or Iterator).
//
// Parameters:
//   - bus: The QueryBus instance where the handler should be registered.
//   - handler: The QueryHandler to register.
//   - opts: Optional HandlerOption values for future customization.
//
// Behavior Details:
//   - The key for storage is generated via fmt.Sprintf("%T|%T").
//   - Currently, handler settings are collected but not persisted.
//
// Example Usage:
//
//	bus := NewQueryBus()
//	RegisterQueryHandler[MyQuery, *MyResult](bus, NewQueryHandlerFunc(func(ctx context.Context, q MyQuery) (*MyResult, error) {
//	    return &MyResult{Value: 42}, nil
//	}))
//
// Generic helper function
func RegisterQueryHandler[T query.Query, R any | Iterator[any]](bus *QueryBus, handler QueryHandler[T, R], opts ...HandlerOption) {
	key := fmt.Sprintf("%T|%T", *new(T), *new(R))
	bus.handlers[key] = handler

	meta := &handlerSettings{}
	for _, opt := range opts {
		opt(meta)
	}
	//bus.settings[key] = meta
}
