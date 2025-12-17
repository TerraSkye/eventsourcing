package eventsourcing

import (
	"context"
	"fmt"
)

// GenericQueryGateway provides a typed interface for executing queries
// registered on a QueryBus. It implements QueryHandler[T,R], allowing
// it to be used wherever a QueryHandler is expected.
//
// Type Parameters:
//   - T: The query type implementing Query.
//   - R: The result type.
//
// Parameters:
//   - bus: The QueryBus to wrap.
//
// Behavior Details:
//   - Lookup of the handler is done at runtime using the query and result types.
//   - Returns an error if no handler is registered or if a type mismatch occurs.
//
// Example Usage:
//
//	bus := NewQueryBus()
//	RegisterQueryHandler[MyQuery, *MyResult](bus, NewQueryHandlerFunc(func(ctx context.Context, q MyQuery) (*MyResult, error) {
//	    return &MyResult{Value: 123}, nil
//	}))
//
//	gateway := NewQueryGateway[MyQuery, *MyResult](bus)
//	result, err := gateway.HandleQuery(context.Background(), MyQuery{ID: "42"})
//	if err != nil {
//	    panic(err)
//	}
//	fmt.Println(result.Value)
type GenericQueryGateway[T Query, R any | Iterator[any]] struct {
	bus *QueryBus
}

// NewQueryGateway creates a typed gateway for a specific query type
// backed by a QueryBus.
//
// Returns:
//   - GenericQueryGateway[T,R]: a typed interface to execute queries.
func NewQueryGateway[T Query, R any | Iterator[any]](bus *QueryBus) GenericQueryGateway[T, R] {
	return GenericQueryGateway[T, R]{bus: bus}
}

// HandleQuery executes the registered handler for a given query.
//
// Parameters:
//   - ctx: The context for the query execution.
//   - qry: The query value of type T.
//
// Returns:
//   - R: The result of the query.
//   - error: Non-nil if no handler is registered or a type mismatch occurs.
func (g GenericQueryGateway[T, R]) HandleQuery(ctx context.Context, qry T) (R, error) {
	key := fmt.Sprintf("%T|%T", qry, *new(R))

	h, ok := g.bus.handlers[key]
	if !ok {
		var zero R
		return zero, fmt.Errorf("no handler registered for query %T -> %T %w", qry, *new(R), ErrHandlerNotFound)
	}

	handler, ok := h.(QueryHandler[T, R])
	if !ok {
		var zero R
		return zero, fmt.Errorf("handler type mismatch for query %T -> %T", qry, *new(R))
	}

	return handler.HandleQuery(ctx, qry)
}
