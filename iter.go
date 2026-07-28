package eventsourcing

import (
	"context"
	"io"
)

// IterFunc produces the next item of type T, returning [io.EOF] once
// iteration is complete.
type IterFunc[T any] func(ctx context.Context) (T, error)

// Iterator is a pull-based iterator over items of type T, driven by an
// [IterFunc] supplied through [NewIteratorFunc] or [NewSliceIterator]. It
// underlies the event streams returned by an [EventStore]'s Load* methods,
// but its next-function can equally paginate, stream, or buffer
// asynchronously — whatever the source needs.
//
// Call Next repeatedly to advance the iterator, reading the current item
// with Value after each call that returns true; when Next returns false,
// call Err to distinguish a clean end of iteration (nil) from a failure. See
// [ExampleNewIteratorFunc].
type Iterator[T any] struct {
	// nextFunc is the function that produces the next value in the iteration.
	// It must return:
	//   - (T, nil) for a valid item
	//   - (any, io.EOF) to signal end of iteration
	//   - (any, error) to signal a failure during iteration
	nextFunc func(ctx context.Context) (T, error)

	// current holds the current item returned by the last Next() call.
	current T

	// err stores the last error encountered during iteration.
	err error

	// done is set when the iteration has completed.
	done bool
}

// Next advances the iterator and reports whether a new value is available.
// It returns false once the underlying [IterFunc] reports [io.EOF] or
// returns any other error; call Err afterward to tell the two apart.
func (it *Iterator[T]) Next(ctx context.Context) bool {
	if it.done || it.err != nil {
		return false
	}

	val, err := it.nextFunc(ctx)
	if err != nil {
		if err == io.EOF {
			it.done = true
			it.err = nil
		} else {
			it.done = true
			it.err = err
		}
		return false
	}

	it.current = val
	return true
}

// Value returns the item read by the most recent call to Next, or the zero
// value of T if Next has not been called or has returned false.
func (it *Iterator[T]) Value() T {
	return it.current
}

// Err returns the error that ended iteration, or nil if Next has not yet
// returned false or iteration completed because the source was exhausted.
func (it *Iterator[T]) Err() error {
	return it.err
}

// All consumes the iterator by calling Next until it returns false,
// collecting each Value along the way, and returns the collected items
// along with the result of Err.
func (it *Iterator[T]) All(ctx context.Context) ([]T, error) {
	var results []T
	for it.Next(ctx) {
		results = append(results, it.Value())
	}
	return results, it.Err()
}

// NewIteratorFunc returns an [Iterator] driven by nextFunc, which must
// return [io.EOF] to signal the end of iteration and any other error to
// signal a failure.
func NewIteratorFunc[T any](nextFunc func(ctx context.Context) (T, error)) *Iterator[T] {
	return &Iterator[T]{nextFunc: nextFunc}
}

// NewSliceIterator returns an [Iterator] that yields the elements of slice
// in order.
func NewSliceIterator[T any](slice []T) *Iterator[T] {
	index := 0

	return &Iterator[T]{
		nextFunc: func(ctx context.Context) (T, error) {
			if index >= len(slice) {
				var zero T
				return zero, io.EOF
			}

			value := slice[index]
			index++
			return value, nil
		},
	}
}
