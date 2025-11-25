package eventsourcing

import (
	"context"
)

// Query is the interface that must be implemented by any type to be considered a query.
type Query interface {
	ID() []byte
}

// QueryHandler represents a handler for a specific query type T and
// produces a result of type R. This interface allows generic, type-safe
// registration and execution of query logic.
//
// Type Parameters:
//   - T: The query type implementing Query.
//   - R: The return type, either a single ReadModel or an Iterator.
//
// Example Usage:
//
//	type MyQuery struct { ID string }
//	type MyResult struct { Value int }
//
//	handler := NewQueryHandlerFunc(func(ctx context.Context, q MyQuery) (*MyResult, error) {
//	    return &MyResult{Value: 123}, nil
//	})
//
//	var _ QueryHandler[MyQuery, *MyResult] = handler
type QueryHandler[T Query, R any | Iterator[any]] interface {
	HandleQuery(ctx context.Context, qry T) (R, error)
}

// queryHandlerFunc is a helper type to allow ordinary functions to
// implement QueryHandler[T,R].
//
// Example Usage:
//
//	handler := NewQueryHandlerFunc(func(ctx context.Context, q MyQuery) (*MyResult, error) {
//	    return &MyResult{Value: 42}, nil
//	})
type queryHandlerFunc[T Query, R any | Iterator[any]] func(ctx context.Context, qry T) (R, error)

// HandleQuery calls the underlying function.
func (f queryHandlerFunc[T, R]) HandleQuery(ctx context.Context, qry T) (R, error) {
	return f(ctx, qry)
}

// NewQueryHandlerFunc creates a QueryHandler from a function.
//
// Parameters:
//   - fn: The function to wrap as a QueryHandler.
//
// Returns:
//   - QueryHandler[T,R]: A handler that implements QueryHandler interface.
//
// Example Usage:
//
//	handler := NewQueryHandlerFunc(func(ctx context.Context, q MyQuery) (*MyResult, error) {
//	    return &MyResult{Value: 42}, nil
//	})
func NewQueryHandlerFunc[T Query, R any | Iterator[any]](fn func(ctx context.Context, qry T) (R, error)) QueryHandler[T, R] {
	return queryHandlerFunc[T, R](fn)
}
