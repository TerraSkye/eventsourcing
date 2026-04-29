package eventsourcing

import (
	"context"
	"fmt"
)

// QueryGateway is a typed, callable facade over QueryBus. Call it directly
// like a function to execute the registered handler for query type T.
// It also implements QueryHandler[T, R], so it can be passed to decorators
// such as WithQueryTelemetry or WithQueryLogging.
//
// Type Parameters:
//   - T: The query type implementing Query.
//   - R: The result type.
//
// Example Usage:
//
//	gateway := NewQueryGateway[MyQuery, *MyResult](bus)
//	result, err := gateway(ctx, MyQuery{ID: "42"})
type QueryGateway[T Query, R any] func(ctx context.Context, qry T) (R, error)

// HandleQuery implements QueryHandler[T, R].
func (g QueryGateway[T, R]) HandleQuery(ctx context.Context, qry T) (R, error) {
	return g(ctx, qry)
}

// GenericQueryGateway is a backwards-compatible alias for QueryGateway.
//
// Deprecated: use QueryGateway directly.
type GenericQueryGateway[T Query, R any] = QueryGateway[T, R]

// NewQueryGateway creates a QueryGateway for a specific query and result type
// backed by a QueryBus. Creating a gateway registers the (T, R) pair as a
// requestee, which is checked by bus.Validate() at startup.
//
// Example Usage:
//
//	listGateway := NewQueryGateway[ListTasks, *TaskList](bus)
//	findGateway := NewQueryGateway[ListTasks, *Task](bus)
func NewQueryGateway[T Query, R any](bus *QueryBus) QueryGateway[T, R] {
	var zero T
	key := fmt.Sprintf("%T|%T", zero, *new(R))
	bus.requestees[key] = struct{}{}

	return func(ctx context.Context, qry T) (R, error) {
		h, ok := bus.handlers[key]
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
}
