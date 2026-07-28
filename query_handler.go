package eventsourcing

import (
	"context"
)

// Query is implemented by any type that can be handled by a [QueryHandler].
type Query interface {
	// ID returns an identifier for this query occurrence, for example for
	// correlation in logs or traces.
	ID() []byte
}

// QueryHandler handles queries of the concrete type T, which must implement
// [Query], and produces a result of type R — typically a read model or an
// [Iterator] over one. It enables generic, type-safe registration and
// execution of query logic through a [QueryBus].
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
type QueryHandler[T Query, R any] interface {
	HandleQuery(ctx context.Context, qry T) (R, error)
}

// queryHandlerFunc adapts an ordinary function to implement
// QueryHandler[T, R].
type queryHandlerFunc[T Query, R any] func(ctx context.Context, qry T) (R, error)

// HandleQuery calls the underlying function.
func (f queryHandlerFunc[T, R]) HandleQuery(ctx context.Context, qry T) (R, error) {
	return f(ctx, qry)
}

// NewQueryHandlerFunc returns fn as a [QueryHandler].
//
// Example Usage:
//
//	handler := NewQueryHandlerFunc(func(ctx context.Context, q MyQuery) (*MyResult, error) {
//	    return &MyResult{Value: 42}, nil
//	})
func NewQueryHandlerFunc[T Query, R any](fn func(ctx context.Context, qry T) (R, error)) QueryHandler[T, R] {
	return queryHandlerFunc[T, R](fn)
}
