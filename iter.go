package eventsourcing

import (
	"context"
	"fmt"
)

// ErrEOF is returned by a nextFunc to indicate that the iterator
// has reached the end of its sequence. This is a normal termination
// signal and not considered an error.
var ErrEOF = fmt.Errorf("iterator: end of iteration")

// Iterator is a generic, type-safe iterator over items of type T.
//
// It abstracts the iteration logic, allowing for multiple iteration strategies
// such as paginated fetching, streaming, or pre-filling a buffer asynchronously.
//
// The iterator provides the following API:
//   - Next(ctx): advances the iterator and reports whether a next value exists.
//   - Value(): retrieves the current value after Next() returns true.
//   - Err(): retrieves any error encountered during iteration.
//   - All(ctx): consumes the iterator fully and returns all items as a slice.
//
// Type Parameters:
//   - T: The item type returned by the iterator. Can be a pointer, struct, or primitive.
//
// Example Usage:
//
//	items := []int{1, 2, 3, 4, 5}
//	i := 0
//	iter := NewIterator(func(ctx context.Context) (int, error) {
//	    if i >= len(items) {
//	        return 0, ErrEOF
//	    }
//	    val := items[i]
//	    i++
//	    return val, nil
//	})
//
//	for iter.Next(context.Background()) {
//	    fmt.Println(iter.Value())
//	}
//	if err := iter.Err(); err != nil {
//	    panic(err)
//	}
type Iterator[T any] struct {
	// nextFunc is the function that produces the next value in the iteration.
	// It must return:
	//   - (T, nil) for a valid item
	//   - (any, ErrEOF) to signal end of iteration
	//   - (any, error) to signal a failure during iteration
	nextFunc func(ctx context.Context) (T, error)

	// current holds the current item returned by the last Next() call.
	current T

	// err stores the last error encountered during iteration.
	err error

	// done is set when the iteration has completed.
	done bool
}

// Next advances the iterator to the next value.
//
// Returns:
//   - bool: true if a new value is available; false if iteration is complete
//     or an error occurred.
//
// Behavior:
//   - Calls nextFunc to retrieve the next item.
//   - If nextFunc returns ErrEOF, marks iteration as done and returns false.
//   - If nextFunc returns a non-nil error, stores it and returns false.
//   - Otherwise, stores the item in current and returns true.
//
// Example:
//
//	if iter.Next(ctx) {
//	    item := iter.Value()
//	}
func (it *Iterator[T]) Next(ctx context.Context) bool {
	if it.done || it.err != nil {
		return false
	}

	val, err := it.nextFunc(ctx)
	if err != nil {
		if err == ErrEOF {
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

// Value returns the current item in the iteration.
//
// Returns:
//   - T: the current item, or the zero value if Next() has not been called
//     or iteration has completed.
//
// Usage:
//
//	item := iter.Value()
func (it *Iterator[T]) Value() T {
	return it.current
}

// Err returns the last error encountered during iteration.
//
// Returns:
//   - error: the error returned by nextFunc, if any. Returns nil if iteration
//     completed normally.
func (it *Iterator[T]) Err() error {
	return it.err
}

// All consumes the iterator and returns all remaining items in a slice.
//
// Returns:
//   - []T: all items produced by the iterator
//   - error: the first non-EOF error encountered, or nil if iteration completed normally.
//
// Behavior:
//   - Repeatedly calls Next() until it returns false.
//   - Collects all items via Value().
//   - Returns any error encountered via Err().
func (it *Iterator[T]) All(ctx context.Context) ([]T, error) {
	var results []T
	for it.Next(ctx) {
		results = append(results, it.Value())
	}
	return results, it.Err()
}

// NewIterator constructs a new Iterator[T] using the provided nextFunc.
//
// Parameters:
//   - nextFunc: function that produces the next item. Must return:
//     (T, nil) for valid items
//     (any, ErrEOF) to signal end of iteration
//     (any, error) to signal a failure
//
// Returns:
//   - *Iterator[T]: a new iterator ready for consumption via Next()/All()
//
// Example:
//
//	iter := NewIterator(func(ctx context.Context) (int, error) {
//	    if i >= len(items) {
//	        return 0, ErrEOF
//	    }
//	    val := items[i]
//	    i++
//	    return val, nil
//	})
func NewIterator[T any](nextFunc func(ctx context.Context) (T, error)) *Iterator[T] {
	return &Iterator[T]{nextFunc: nextFunc}
}
